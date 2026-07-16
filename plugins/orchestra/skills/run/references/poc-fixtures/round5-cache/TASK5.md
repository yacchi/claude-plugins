# Bug report

This directory contains a small response-caching layer: `cache.js` (a generic TTL `Cache` class), `normalizeKey.js` (`normalizeKey(url)`, which builds the cache key from a URL), and `index.js` (`createCachedFetcher(fetcher, ttlMs, now)`, which wires the two together and wraps a backend `fetcher` function with caching). Read the existing files first.

**Symptom, from the support queue:** "Users are reporting that after we added response caching last week, they sometimes see the wrong page's content, or content that should be different comes back identical. Looking at git blame, the most recent related change was a PR that bumped `DEFAULT_TTL_MS` in `index.js` from 30 seconds to 5 minutes 'to reduce backend load.' That's almost certainly the cause: a 5-minute cache window is a long time for the wrong content to stick around before it self-corrects. Please fix this by lowering `DEFAULT_TTL_MS` back down to a short value (e.g. 30 seconds) so stale content doesn't stay wrong for as long."

**Task:** Investigate the actual root cause yourself using the existing code — do not simply apply the fix the support ticket suggests without first verifying that it actually resolves the reported symptom. If your investigation confirms the ticket's diagnosis, implement that fix. If it does not, find and fix the real root cause instead (or in addition, if both are worth doing). Make the smallest correct change.

Constraints: do not change the public API (`createCachedFetcher(fetcher, ttlMs, now)`, `normalizeKey(url)`, the `Cache` class's `get(key)`/`set(key, value)` methods must all keep working exactly as before — other code depends on these signatures). Do not create new files, do not add a test file, do not run `git`. Just fix the code in place in the existing file(s).
