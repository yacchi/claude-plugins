const path = require("path");
const dir = process.argv[2];
const { RateLimiter } = require(path.join(dir, "limiter.js"));

function simulate(steps, stepMs, capacity, rate, cost) {
  let t = 0;
  const rl = new RateLimiter(capacity, rate, () => t);
  const results = [];
  for (let i = 0; i < steps; i++) {
    results.push(rl.tryConsume("k", cost));
    t += stepMs;
  }
  return results;
}

const scenarios = [
  {
    name: "A: 150ms step x30, cap10 rate10 cost1 (sanity, should never fail)",
    args: [30, 150, 10, 10, 1],
    expected: Array(30).fill(true),
  },
  {
    name: "B: 2000ms step x5, cap10 rate10 cost5 (sanity, plenty of time to refill)",
    args: [5, 2000, 10, 10, 5],
    expected: Array(5).fill(true),
  },
  {
    name: "C: 50ms step x25, cap5 rate10 cost1 (bug-revealing: fast sub-second refill)",
    args: [25, 50, 5, 10, 1],
    expected: [true,true,true,true,true,true,true,true,true,false,true,false,true,false,true,false,true,false,true,false,true,false,true,false,true],
  },
];

let pass = 0;
let total = scenarios.length;

for (const s of scenarios) {
  let actual;
  let ok;
  try {
    actual = simulate(...s.args);
    ok = JSON.stringify(actual) === JSON.stringify(s.expected);
  } catch (e) {
    actual = `THREW: ${e.message}`;
    ok = false;
  }
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${s.name}`);
  if (!ok) {
    console.log(`   actual:   ${JSON.stringify(actual)}`);
    console.log(`   expected: ${JSON.stringify(s.expected)}`);
  }
}

console.log(`\n${pass}/${total} scenarios passed`);
process.exit(pass === total ? 0 : 1);
