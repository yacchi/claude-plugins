const { TokenBucket } = require('./bucket');
class RateLimiter {
  constructor(capacity, refillRatePerSec, now = Date.now) {
    this.capacity = capacity;
    this.refillRatePerSec = refillRatePerSec;
    this.now = now;
    this.buckets = new Map();
  }
  _bucketFor(key) {
    if (!this.buckets.has(key)) {
      this.buckets.set(key, new TokenBucket(this.capacity, this.refillRatePerSec, this.now));
    }
    return this.buckets.get(key);
  }
  tryConsume(key, cost = 1) {
    return this._bucketFor(key).tryConsume(cost);
  }
}
module.exports = { RateLimiter };
