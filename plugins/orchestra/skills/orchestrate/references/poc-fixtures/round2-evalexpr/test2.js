const path = require("process").argv[2];
const { evaluate } = require(path);

const cases = [
  ["1+2", 3],
  ["2*3+4", 10],
  ["2+3*4", 14],
  ["(2+3)*4", 20],
  ["2-3-4", -5], // left-assoc minus
  ["10/2/5", 1], // left-assoc div
  ["2^3^2", 512], // right-assoc exponent: 2^(3^2)
  ["(2^3)^2", 64], // explicit left grouping, sanity check
  ["-2^2", -4], // unary minus binds looser than ^
  ["(-2)^2", 4], // explicit grouping forces the other reading
  ["-3+5", 2],
  ["3.5*2", 7],
  ["  2  +   3  ", 5], // whitespace
  ["-(1+2)*3", -9],
];

const errorCases = [
  ["5/0", "Division by zero"],
  ["0/0", "Division by zero"],
  ["2++2", "Invalid expression"],
  ["2+", "Invalid expression"],
  ["(2+3", "Invalid expression"],
  ["2+3)", "Invalid expression"],
  ["", "Invalid expression"],
  ["2**2", "Invalid expression"],
];

let pass = 0;
let total = 0;

for (const [input, expected] of cases) {
  total++;
  let actual;
  let ok;
  try {
    actual = evaluate(input);
    ok = Math.abs(actual - expected) < 1e-9;
  } catch (e) {
    actual = `THREW: ${e.message}`;
    ok = false;
  }
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  evaluate(${JSON.stringify(input)}) = ${JSON.stringify(actual)}  expected ${expected}`);
}

for (const [input, expectedMessage] of errorCases) {
  total++;
  let ok;
  let actual;
  try {
    const v = evaluate(input);
    actual = `NO THROW, returned ${v}`;
    ok = false;
  } catch (e) {
    actual = e.message;
    ok = e.message === expectedMessage;
  }
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  evaluate(${JSON.stringify(input)}) threw ${JSON.stringify(actual)}  expected message ${JSON.stringify(expectedMessage)}`);
}

console.log(`\n${pass}/${total} passed`);
process.exit(pass === total ? 0 : 1);
