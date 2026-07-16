# Task

Create a file named `formatBytes.js` in this exact directory (do not create subdirectories, do not run tests, do not use git).

It must export a single CommonJS function:

```js
function formatBytes(bytes) {
  // ...
}
module.exports = { formatBytes };
```

Spec:
- Input is a non-negative integer number of bytes.
- Output is a human-readable string using binary (1024-based) units: B, KiB, MiB, GiB, TiB.
- Round to at most 2 decimal places, but do not print trailing zeros (e.g. "1 MiB" not "1.00 MiB", "1.5 MiB" not "1.50 MiB").
- Pick the largest unit such that the displayed value is < 1024, EXCEPT when rounding would carry the value up to 1024 in the current unit — in that case, roll over to the next unit instead. For example, 1048575 bytes is 1023.999... KiB, which must NOT print as "1024 KiB"; it must print as "1 MiB".
- 0 bytes must print as "0 B".

Write only the file. Do not explain your reasoning at length; just write the code.
