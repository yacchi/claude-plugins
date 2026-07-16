const assert = require("assert");
const path = require("process").argv[2];
const { parseJson } = require(path);

const validCases = [
  ['"hello"', "hello"],
  ['""', ""],
  ["0", 0],
  ["-0", -0],
  ["0.5", 0.5],
  ["-0.5", -0.5],
  ["10", 10],
  ["1.5e+3", 1500],
  ["1E-2", 0.01],
  ["true", true],
  ["false", false],
  ["null", null],
  ["[]", []],
  ["{}", {}],
  ["[1,2,3]", [1, 2, 3]],
  ['{"a":1,"b":2}', { a: 1, b: 2 }],
  ['{"a":{"b":[1,2,{"c":true}]}}', { a: { b: [1, 2, { c: true }] } }],
  ['"line1\\nline2\\ttab\\\\back\\"quote"', 'line1\nline2\ttab\\back"quote'],
  ['"\\uD83D\\uDE00"', "😀"], // surrogate pair -> emoji
  ['"\\u0041"', "A"],
  ["  [ 1 , 2 ]  ", [1, 2]], // whitespace tolerance
  ['{"a":1,"a":2}', { a: 2 }], // duplicate key, last wins
  ["[1,2,3]", [1, 2, 3]],
];

const invalidCases = [
  "123abc",
  "{} {}",
  "[1]1",
  "01",
  "+1",
  "1.",
  ".5",
  '{a:1}', // unquoted key
  '{"a":1,}', // trailing comma
  "[1,2,]", // trailing comma
  "True",
  "",
  "   ",
  '"unterminated',
  '"bad\nstring"', // raw control char in string
  "[1,,2]",
  "{,}",
];

let pass = 0;
let total = 0;

function deepEqual(a, b) {
  try {
    assert.deepStrictEqual(a, b);
    return true;
  } catch {
    return false;
  }
}

for (const [input, expected] of validCases) {
  total++;
  let actual;
  let ok;
  try {
    actual = parseJson(input);
    ok = deepEqual(actual, expected);
  } catch (e) {
    actual = `THREW: ${e.message}`;
    ok = false;
  }
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  parseJson(${JSON.stringify(input)}) = ${JSON.stringify(actual)}  expected ${JSON.stringify(expected)}`);
}

for (const input of invalidCases) {
  total++;
  let ok;
  let actual;
  try {
    const v = parseJson(input);
    actual = `NO THROW, returned ${JSON.stringify(v)}`;
    ok = false;
  } catch (e) {
    actual = `threw: ${e.message}`;
    ok = true;
  }
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  parseJson(${JSON.stringify(input)}) ${actual}  (expected: throw)`);
}

console.log(`\n${pass}/${total} passed`);
process.exit(pass === total ? 0 : 1);
