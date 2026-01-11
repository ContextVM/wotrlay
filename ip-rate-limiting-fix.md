# IP Rate Limiting Fix - Design Document

## Problem Statement

High-ranked users (e.g., rank 0.95) behind VPNs or shared IPs are incorrectly receiving rank=0 when their IP group exceeds the `RANK_QUEUE_IP_DAILY_LIMIT`. This causes them to be treated as low-trust users, triggering URL policy restrictions and kind gating.

### Root Cause

The current implementation in [`main.lookupRank()`](main.go:453) applies IP-group rate limiting **before** attempting rank refresh. When:
1. A pubkey has no cached rank (cache miss)
2. The IP group is rate-limited
3. The function returns rank=0

This rank=0 then triggers URL policy and kind restrictions in [`main.handleEvent()`](main.go:397).

### Reproduction Case

```bash
# Set low rank queue limit to trigger IP rate limiting
export RANK_QUEUE_IP_DAILY_LIMIT=10

# Send events from random pubkeys to exhaust IP limit
# Then send from high-rank pubkey with URL in content
# Result: "url-not-allowed: only text notes without URLs"
```

## Proposed Solutions

### Option A: Decouple Publish Rank Refresh from IP Gate (Recommended)

**Approach**: Remove IP-group rate limiting from the synchronous publish path and replace with:
- Global relay-wide limiter for rank provider protection
- Existing cache mechanisms for per-pubkey optimization
- Preserve-on-failure logic for stale data reuse

**Integration Points**:
1. [`main.lookupRank()`](main.go:453) - Remove IP gate, add global limiter check
2. [`main.loadConfig()`](main.go:113) - Add `GLOBAL_RANK_REFRESH_LIMIT` configuration
3. [`rate.go`](rate.go) - Add global limiter instance

**Implementation Details**:
```go
// In lookupRank(), replace IP gate with global limiter
if limiter.Allow("global-rank-refresh", cfg.GlobalRankRefreshLimit, cfg.GlobalRankRefreshLimit/secondsPerDay) {
    // Attempt synchronous refresh
    refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()
    if refreshed, err := cache.GetRank(refreshCtx, pubkey); err == nil {
        return refreshed
    }
    // Preserve stale data on failure
    if rank, exists := cache.Rank(pubkey); exists {
        return rank
    }
} else {
    // Global limit hit, check for stale data
    if rank, exists := cache.Rank(pubkey); exists {
        return rank
    }
}
return 0
```

**Security Analysis**:
- No new abuse vectors introduced
- Global limiter provides better protection than IP-based (more precise)
- Low-rank users still cannot publish URLs or non-kind-1 events
- High-rank users behind VPNs are no longer penalized

**Configuration Changes**:
```bash
# Remove or deprecate
RANK_QUEUE_IP_DAILY_LIMIT=250

# Add
GLOBAL_RANK_REFRESH_LIMIT=500  # requests per second, relay-wide
```

### Option B: Persistent Rank Cache (Complementary)

**Approach**: Persist rank cache to disk (Badger) to survive restarts and reduce cache misses.

**Integration Points**:
1. [`rank.go`](rank.go) - Add load/save methods
2. [`main.go`](main.go) - Initialize cache from disk on startup
3. [`rank.go`](rank.go) - Write-through cache updates to disk

**Implementation Details**:
- Store ranks in Badger with TTL (e.g., 7 days)
- On startup: load recent ranks into LRU cache
- On update: write-through to both LRU and Badger
- On cache miss with stale data in Badger: load into LRU

**Benefits**:
- Reduces cache misses after restart
- Provides last-known rank when provider is unreachable
- Complements Option A by reducing "unknown rank" cases

## Recommended Implementation Path

**Phase 1: Option A (Immediate Fix)**
- Remove IP gate from publish path
- Add global limiter
- Keep existing cache mechanisms
- No per-pubkey backoff needed (cache + singleflight sufficient)

**Phase 2: Option B (Production Hardening)**
- Add persistent cache storage
- Reduce cache misses across restarts
- Improve user experience

## Code to Remove/Deprecate

### In [`main.go`](main.go):
```go
// Remove these constants/lines:
const rankQueueKeyPrefix = "rank-queue:"  // Line 82

// In lookupRank() (lines 466-468):
ipGroup := c.IP().Group()
rankQueueKey := rankQueueKeyPrefix + ipGroup
if limiter.Allow(rankQueueKey, cfg.RankQueueIPDailyLimit, cfg.RankQueueIPDailyLimit/secondsPerDay) {
    // ...
}

// Remove RANK_QUEUE_IP_DAILY_LIMIT from Config struct (line 46)
// Remove from loadConfig() (line 136)
// Remove from .env.example
```

### In [`rate.go`](rate.go):
No changes needed - existing limiter can handle global limit key.

Note: Prefer removing than deprecating. cleaning up code