const path = require("process").argv[2];
const { formatBytes } = require(path);

const cases = [
  [0, "0 B"],
  [1, "1 B"],
  [1023, "1023 B"],
  [1024, "1 KiB"],
  [1536, "1.5 KiB"],
  [1048575, "1 MiB"],
  [1048576, "1 MiB"],
  [1073741823, "1 GiB"],
  [1073741824, "1 GiB"],
  [5 * 1024 * 1024, "5 MiB"],
];

let pass = 0;
for (const [input, expected] of cases) {
  let actual;
  try {
    actual = formatBytes(input);
  } catch (e) {
    actual = `THREW: ${e.message}`;
  }
  const ok = actual === expected;
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  formatBytes(${input}) = ${JSON.stringify(actual)}  expected ${JSON.stringify(expected)}`);
}
console.log(`\n${pass}/${cases.length} passed`);
process.exit(pass === cases.length ? 0 : 1);
