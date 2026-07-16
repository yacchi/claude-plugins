# Task

Create a file named `parseJson.js` in this exact directory (do not create subdirectories, do not run tests, do not use git).

It must export a single CommonJS function:

```js
function parseJson(text) {
  // ...
}
module.exports = { parseJson };
```

Spec: `parseJson` is a from-scratch implementation of a strict JSON parser. It takes a JSON text string and returns the equivalent JavaScript value (object/array/string/number/boolean/null).

**Hard constraint: do not use `JSON.parse`, `eval`, `Function`, or any parsing/JSON library. Implement the tokenizer and parser yourself.**

Follow the JSON spec (RFC 8259) strictly, including these traps that naive implementations get wrong:

- **Trailing content is invalid.** After the top-level value, only whitespace may follow. `"123abc"`, `"{} {}"`, `"[1]1"` must all throw.
- **Numbers**: integer part must not have leading zeros, except the integer part being exactly `0` (`"01"` is invalid, `"0"` and `"0.5"` and `"-0"` are valid, `"10"` is valid). A leading `+` is invalid (`"+1"` throws). A decimal point must have digits on both sides (`"1."` and `".5"` both throw). Exponents (`e`/`E`, optional `+`/`-`, digits) are supported, e.g. `"1.5e+3"` → `1500`, `"1E-2"` → `0.01`.
- **Strings**: support escape sequences `\" \\ \/ \b \f \n \r \t` and `\uXXXX` unicode escapes, including surrogate pairs (`"😀"` must decode to the 😀 emoji, i.e. code point U+1F600). A raw, unescaped control character (e.g. a literal newline byte) inside a string is invalid and must throw.
- **Objects**: keys must be double-quoted strings (unquoted keys are invalid). No trailing comma before `}` (`{"a":1,}` throws). If the same key appears twice, the last occurrence wins (matching `JSON.parse`'s behavior).
- **Arrays**: no trailing comma before `]` (`[1,2,]` throws).
- **Literals**: `true`, `false`, `null` are case-sensitive (`"True"` throws).
- **Whitespace** (space, tab, `\n`, `\r`) is allowed between tokens and must be ignored there.
- Empty input, or input that is only whitespace, is invalid and must throw.

On any invalid input, throw a JavaScript `Error` (any message is fine — this task does not require an exact message string, just that it throws instead of silently returning something wrong or partially parsing).

Write only the file. Do not explain your reasoning at length; just write the code.
