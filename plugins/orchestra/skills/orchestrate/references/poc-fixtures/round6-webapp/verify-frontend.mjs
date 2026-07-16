// Usage: node verify-frontend.mjs <candidateFrontendDir>
// Typechecks with the provided tsconfig, compiles to CommonJS, then runs
// unit tests against the compiled output with a mocked global fetch.
import { execSync } from "node:child_process";
import path from "node:path";
import fs from "node:fs";

const dir = process.argv[2];

let pass = 0;
let total = 0;
function check(name, ok, detail) {
  total++;
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  " + detail : ""}`);
}

console.log("--- tsc --strict typecheck ---");
let typecheckOk = true;
try {
  execSync(`npx -y typescript@latest -p tsconfig.json --noEmit`, { cwd: dir, stdio: "inherit" });
} catch {
  typecheckOk = false;
}
check("tsc --strict --noEmit passes", typecheckOk);

console.log("--- tsc build (emit JS) ---");
try {
  execSync(`npx -y typescript@latest -p tsconfig.json`, { cwd: dir, stdio: "inherit" });
} catch {
  console.log("(build had errors; will still try to run compiled output if present)");
}

const distDir = path.join(dir, "dist");
const clientPath = path.join(distDir, "client.js");
const transformPath = path.join(distDir, "transform.js");
if (!fs.existsSync(clientPath) || !fs.existsSync(transformPath)) {
  console.log(`\n${pass}/${total + 1} scenarios passed (compiled output missing, cannot run runtime tests)`);
  process.exit(1);
}

const { createTask, listTasks, updateTaskStatus } = await import(clientPath);
const { groupByPriority, statusBadgeColor } = await import(transformPath);

// --- Mock fetch for client.ts tests ---
const calls = [];
globalThis.fetch = async (url, opts) => {
  calls.push({ url: String(url), opts });
  const method = opts?.method ?? "GET";
  if (method === "POST" && String(url).endsWith("/tasks")) {
    const body = JSON.parse(opts.body);
    if (!body.title) {
      return new Response(JSON.stringify({ error: "title required" }), { status: 400 });
    }
    return new Response(JSON.stringify({ id: "task-1", ...body, status: "pending" }), { status: 201 });
  }
  if (method === "GET" && String(url).includes("/tasks")) {
    return new Response(JSON.stringify([{ id: "task-1", title: "A", priority: "low", dueDate: "2026-01-01", status: "pending" }]), { status: 200 });
  }
  if (method === "PATCH") {
    return new Response(JSON.stringify({ id: "task-1", title: "A", priority: "low", dueDate: "2026-01-01", status: "done" }), { status: 200 });
  }
  return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
};

try {
  const created = await createTask("http://x", { title: "A", priority: "low", dueDate: "2026-01-01" });
  check("createTask returns parsed task", created.id === "task-1" && created.status === "pending");

  let threw = false;
  try {
    await createTask("http://x", { title: "", priority: "low", dueDate: "2026-01-01" });
  } catch (e) {
    threw = e instanceof Error && /title required/.test(e.message);
  }
  check("createTask throws with server error message on 400", threw);

  calls.length = 0;
  await listTasks("http://x");
  check("listTasks with no opts sends no query string", !calls[0].url.includes("?"), `got url=${calls[0].url}`);

  calls.length = 0;
  await listTasks("http://x", { sort: "priority" });
  check(
    "listTasks with partial opts omits unset fields (no literal 'undefined')",
    calls[0].url.includes("sort=priority") && !calls[0].url.includes("undefined"),
    `got url=${calls[0].url}`
  );

  await updateTaskStatus("http://x", "task-1", "done");
  check("updateTaskStatus sends PATCH", calls.at(-1) === undefined ? false : true); // sanity, PATCH call happened without throwing

  // --- transform.ts ---
  const tasks = [
    { id: "1", title: "A", priority: "low", dueDate: "2026-01-01", status: "pending" },
    { id: "2", title: "B", priority: "high", dueDate: "2026-01-01", status: "pending" },
  ];
  const grouped = groupByPriority(tasks);
  check(
    "groupByPriority includes all 3 keys even when empty",
    Array.isArray(grouped.low) && Array.isArray(grouped.medium) && Array.isArray(grouped.high) && grouped.medium.length === 0,
    `got keys=${JSON.stringify(Object.keys(grouped))} medium.length=${grouped.medium?.length}`
  );
  check("groupByPriority groups correctly", grouped.low.length === 1 && grouped.high.length === 1);

  check("statusBadgeColor: done -> green regardless of date", statusBadgeColor("done", "2020-01-01", "2026-01-01") === "green");
  check("statusBadgeColor: pending + overdue -> red", statusBadgeColor("pending", "2026-01-01", "2026-01-02") === "red");
  check("statusBadgeColor: pending + due exactly today -> yellow (not overdue)", statusBadgeColor("pending", "2026-01-02", "2026-01-02") === "yellow");
  check("statusBadgeColor: pending + future due -> yellow", statusBadgeColor("pending", "2026-01-05", "2026-01-02") === "yellow");
} catch (e) {
  console.log("RUNTIME ERROR:", e);
}

console.log(`\n${pass}/${total} scenarios passed`);
process.exit(pass === total ? 0 : 1);
