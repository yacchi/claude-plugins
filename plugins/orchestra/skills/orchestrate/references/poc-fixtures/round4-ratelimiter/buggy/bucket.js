class TokenBucket {
  constructor(capacity, refillRatePerSec, now = Date.now) {
    this.capacity = capacity;
    this.refillRatePerSec = refillRatePerSec;
    this.tokens = capacity;
    this.now = now;
    this.lastRefillMs = this.now();
  }
  _refill() {
    const nowMs = this.now();
    const elapsedSec = Math.floor((nowMs - this.lastRefillMs) / 1000); // BUG
    if (elapsedSec > 0) {
      this.tokens = Math.min(this.capacity, this.tokens + elapsedSec * this.refillRatePerSec);
      this.lastRefillMs = nowMs;
    }
  }
  tryConsume(cost = 1) {
    this._refill();
    if (this.tokens >= cost) {
      this.tokens -= cost;
      return true;
    }
    return false;
  }
}
module.exports = { TokenBucket };
