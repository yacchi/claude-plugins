class Cache {
  constructor(ttlMs, now = Date.now) {
    this.ttlMs = ttlMs;
    this.now = now;
    this.store = new Map();
  }
  get(key) {
    const entry = this.store.get(key);
    if (!entry) return undefined;
    if (this.now() >= entry.expiresAt) {
      this.store.delete(key);
      return undefined;
    }
    return entry.value;
  }
  set(key, value) {
    this.store.set(key, { value, expiresAt: this.now() + this.ttlMs });
  }
}
module.exports = { Cache };
