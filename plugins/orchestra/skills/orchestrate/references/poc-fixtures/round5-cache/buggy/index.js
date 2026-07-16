const { Cache } = require('./cache');
const { normalizeKey } = require('./normalizeKey');

const DEFAULT_TTL_MS = 5 * 60 * 1000; // 5 minutes -- recently changed from 30s, see bug report

function createCachedFetcher(fetcher, ttlMs = DEFAULT_TTL_MS, now = Date.now) {
  const cache = new Cache(ttlMs, now);
  return function getCached(url) {
    const key = normalizeKey(url);
    const cached = cache.get(key);
    if (cached !== undefined) return cached;
    const value = fetcher(url);
    cache.set(key, value);
    return value;
  };
}
module.exports = { createCachedFetcher, DEFAULT_TTL_MS };
