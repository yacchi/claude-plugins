const path = require("path");
const dir = process.argv[2];
const { createCachedFetcher } = require(path.join(dir, "index.js"));

let pass = 0;
let total = 0;

function check(name, ok, detail) {
  total++;
  if (ok) pass++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  " + detail : ""}`);
}

// Frozen fake clock: TTL is structurally irrelevant to every check below,
// since time never advances during the test. Any fix that only touches
// DEFAULT_TTL_MS and does not touch normalizeKey cannot pass scenario 2.
let t = 0;
const calls = [];
const fetcher = (url) => {
  calls.push(url);
  return "content-for:" + url;
};
const getCached = createCachedFetcher(fetcher, 5 * 60 * 1000, () => t);

try {
  // Scenario 1: safe tracking params (utm_source) should still collapse to one cache entry.
  const a1 = getCached("/product/123?utm_source=newsletter");
  const a2 = getCached("/product/123?utm_source=twitter");
  check(
    "Scenario 1: different utm_source values share a cache entry",
    a1 === a2 && calls.length === 1,
    `a1=${JSON.stringify(a1)} a2=${JSON.stringify(a2)} fetcherCalls=${calls.length}`
  );

  // Scenario 2 (the actual root cause): content-affecting params must NOT collapse.
  const callsBefore = calls.length;
  const b1 = getCached("/product/123?page=1");
  const b2 = getCached("/product/123?page=2");
  check(
    "Scenario 2: different page= values return different content (root-cause check)",
    b1 !== b2 && b1 === "content-for:/product/123?page=1" && b2 === "content-for:/product/123?page=2" && calls.length === callsBefore + 2,
    `b1=${JSON.stringify(b1)} b2=${JSON.stringify(b2)} fetcherCalls=${calls.length}`
  );

  // Scenario 3: identical URL twice is still a cache hit (don't over-correct into no caching at all).
  const callsBefore2 = calls.length;
  const c1 = getCached("/product/999");
  const c2 = getCached("/product/999");
  check(
    "Scenario 3: identical URL is still cached (fetcher called once)",
    c1 === c2 && calls.length === callsBefore2 + 1,
    `c1=${JSON.stringify(c1)} c2=${JSON.stringify(c2)} fetcherCalls=${calls.length}`
  );
} catch (e) {
  console.log(`FAIL  threw: ${e.message}`);
  total = Math.max(total, 3);
}

console.log(`\n${pass}/${total} scenarios passed`);
process.exit(pass === total ? 0 : 1);
