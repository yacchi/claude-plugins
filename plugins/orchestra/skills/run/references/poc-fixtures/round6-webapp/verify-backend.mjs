// Usage: node verify-backend.mjs <candidateBackendDir>
// Builds and runs the candidate's Go server as a subprocess, hits it over HTTP,
// and independently checks response correctness. Never trusts anything other
// than what actually comes back over the wire.
import { spawn, execSync } from "node:child_process";
import path from "node:path";

const dir = process.argv[2];
const PORT = 8091;
const BASE = `http://127.0.0.1:${PORT}`;

let pass = 0;
let total = 0;
function check(name, ok, detail) {
  total++;
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  " + detail : ""}`);
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function main() {
  console.log("--- go build ---");
  execSync("go build -o server .", { cwd: dir, stdio: "inherit" });
  console.log("--- go vet ---");
  try {
    execSync("go vet ./...", { cwd: dir, stdio: "inherit" });
  } catch {
    console.log("(go vet reported issues, continuing anyway)");
  }

  const child = spawn(path.join(dir, "server"), [], {
    cwd: dir,
    env: { ...process.env, PORT: String(PORT) },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let ready = false;
  child.stdout.on("data", () => { ready = true; });
  child.stderr.on("data", (d) => process.stderr.write(d));

  for (let i = 0; i < 50 && !ready; i++) await sleep(100);
  await sleep(300);

  try {
    // --- Validation ---
    let r = await fetch(`${BASE}/tasks`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title: "", priority: "low", dueDate: "2026-01-01" }) });
    check("reject empty title", r.status === 400);

    r = await fetch(`${BASE}/tasks`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title: "x", priority: "urgent", dueDate: "2026-01-01" }) });
    check("reject invalid priority", r.status === 400);

    r = await fetch(`${BASE}/tasks`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title: "x", priority: "low", dueDate: "01-01-2026" }) });
    check("reject malformed date", r.status === 400);

    // --- Create a known set of tasks ---
    const specs = [
      { title: "A", priority: "medium", dueDate: "2026-03-10" },
      { title: "B", priority: "high", dueDate: "2026-03-05" },
      { title: "C", priority: "low", dueDate: "2026-03-05" }, // same dueDate as B, different priority
      { title: "D", priority: "high", dueDate: "2026-03-01" },
      { title: "E", priority: "low", dueDate: "2026-03-20" },
    ];
    const created = [];
    for (const s of specs) {
      const cr = await fetch(`${BASE}/tasks`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(s) });
      const body = await cr.json();
      check(`create "${s.title}" succeeds (201)`, cr.status === 201 && body.status === "pending" && typeof body.id === "string" && body.id.length > 0);
      created.push(body);
    }

    // --- sort=priority, order=desc: high(D 03-01, B 03-05) > medium(A) > low(C 03-05, E 03-20), ties broken by dueDate asc ---
    r = await fetch(`${BASE}/tasks?sort=priority&order=desc`);
    let list = await r.json();
    let titles = list.map((t) => t.title);
    check(
      "sort=priority order=desc is semantic (high>medium>low), tie-broken by dueDate asc",
      JSON.stringify(titles) === JSON.stringify(["D", "B", "A", "C", "E"]),
      `got ${JSON.stringify(titles)}`
    );

    // --- sort=priority, order=asc: low(C,E) < medium(A) < high(D,B), same tie-break rule ---
    r = await fetch(`${BASE}/tasks?sort=priority&order=asc`);
    list = await r.json();
    titles = list.map((t) => t.title);
    check(
      "sort=priority order=asc is semantic, tie-broken by dueDate asc",
      JSON.stringify(titles) === JSON.stringify(["C", "E", "A", "D", "B"]),
      `got ${JSON.stringify(titles)}`
    );

    // --- default list (no params): dueDate ascending ---
    r = await fetch(`${BASE}/tasks`);
    list = await r.json();
    titles = list.map((t) => t.title);
    check(
      "default GET /tasks sorts by dueDate ascending",
      JSON.stringify(titles) === JSON.stringify(["D", "B", "C", "A", "E"]),
      `got ${JSON.stringify(titles)}`
    );

    // --- repeat the same GET again: must be deterministic (catches map-iteration nondeterminism) ---
    r = await fetch(`${BASE}/tasks`);
    const list2 = await r.json();
    check(
      "GET /tasks order is deterministic across repeated calls",
      JSON.stringify(list.map((t) => t.id)) === JSON.stringify(list2.map((t) => t.id))
    );

    // --- PATCH ---
    const target = created[0];
    r = await fetch(`${BASE}/tasks/${target.id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status: "done" }) });
    const patched = await r.json();
    check("PATCH status to done succeeds", r.status === 200 && patched.status === "done");

    r = await fetch(`${BASE}/tasks/${target.id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status: "bogus" }) });
    check("PATCH rejects invalid status", r.status === 400);

    r = await fetch(`${BASE}/tasks/does-not-exist`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status: "done" }) });
    check("PATCH on unknown id returns 404", r.status === 404);
  } finally {
    child.kill();
  }

  console.log(`\n${pass}/${total} scenarios passed`);
  process.exit(pass === total ? 0 : 1);
}

main().catch((e) => {
  console.error("HARNESS ERROR:", e);
  process.exit(2);
});
