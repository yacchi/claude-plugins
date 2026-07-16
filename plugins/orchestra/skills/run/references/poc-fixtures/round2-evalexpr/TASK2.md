# Task

Create a file named `evalExpr.js` in this exact directory (do not create subdirectories, do not run tests, do not use git).

It must export a single CommonJS function:

```js
function evaluate(expr) {
  // ...
}
module.exports = { evaluate };
```

Spec: `evaluate` parses and computes a numeric arithmetic expression given as a string, and returns a JavaScript `number`.

Supported syntax:
- Integer and decimal numeric literals (e.g. `3`, `3.5`).
- Binary operators `+ - * /` with standard precedence (`*` and `/` bind tighter than `+` and `-`), left-associative.
- Binary operator `^` for exponentiation, which binds tighter than `* /`, and is RIGHT-associative: `2^3^2` must evaluate as `2^(3^2)` = 512, not `(2^3)^2` = 64.
- Unary minus, e.g. `-3` or `-(1+2)`. Unary minus binds LOOSER than `^`: `-2^2` must evaluate as `-(2^2)` = -4, NOT `(-2)^2` = 4. This matches standard mathematical convention and most programming languages.
- Parentheses for grouping.
- Arbitrary whitespace between tokens is allowed and must be ignored.

Error handling (throw a JavaScript `Error` with exactly this message string, no other text):
- Division by zero (`x / 0` for any `x`): throw `new Error("Division by zero")`.
- Any malformed expression (unmatched parentheses, empty input, consecutive operators like `2++2` or `2**2`, a trailing operator like `2+`, an operator with no left operand except a leading unary minus, invalid characters): throw `new Error("Invalid expression")`.

Do not use `eval`, `Function`, or any parsing/math library — implement the parser yourself. Write only the file, do not explain your reasoning at length; just write the code.
