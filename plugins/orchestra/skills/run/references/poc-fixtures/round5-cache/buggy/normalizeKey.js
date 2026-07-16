const SAFE_TO_STRIP_PARAMS = new Set(['utm_source', 'utm_medium', 'utm_campaign', 'ref', 'fbclid']);
function normalizeKey(url) {
  const [path] = url.split('?'); // BUG: drops ALL query params unconditionally
  return path;
}
module.exports = { normalizeKey, SAFE_TO_STRIP_PARAMS };
