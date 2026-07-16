const SAFE_TO_STRIP_PARAMS = new Set(['utm_source', 'utm_medium', 'utm_campaign', 'ref', 'fbclid']);
function normalizeKey(url) {
  const [path, query] = url.split('?');
  if (!query) return path;
  const params = new URLSearchParams(query);
  const kept = [];
  for (const [k, v] of params) {
    if (!SAFE_TO_STRIP_PARAMS.has(k)) kept.push(`${k}=${v}`);
  }
  kept.sort();
  return kept.length ? `${path}?${kept.join('&')}` : path;
}
module.exports = { normalizeKey, SAFE_TO_STRIP_PARAMS };
