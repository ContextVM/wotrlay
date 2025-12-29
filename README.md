![wotrlay](https://blossom.band/2e2664e11cc3cf69e4488beca8ae0875845ad166d982feb61bb111bdc5304b5d.png)

# wotrlay

A Web-of-Trust (WoT) based Nostr relay with reputation-driven rate limiting.

## Overview

wotrlay enforces community spam-protection using external trust scores (`r ∈ [0,1]`). Rate limits are determined by a pubkey's reputation, with higher trust scores granting more publishing capacity and additional privileges.

## Key Features

- **Trust-tiered rate limiting**: Publishing capacity scales with reputation
- **Kind gating**: Only Kind 1 events allowed below trust threshold
- **True token bucket**: Smooth, continuous refill (not daily reset)
- **Backfill support**: High-trust pubkeys can migrate old history without throttling
- **No NIP-42 required**: Rate limiting based on `event.PubKey`
- **Minimal IP limiting**: Only protects rank-queue operations

## Rate Limit Tiers

When `HIGH_THRESHOLD` is set (e.g., 0.9):

| Tier | Trust Score | Kinds Allowed | Daily Rate |
|------|-------------|---------------|------------|
| A    | r = 0       | Kind 1 only   | 1          |
| B    | 0 < r < 0.5 | Kind 1 only   | 1-100      |
| C    | 0.5 ≤ r < 0.9 | All kinds   | 100-5000   |
| D    | r ≥ 0.9     | All kinds     | 10,000     |

When `HIGH_THRESHOLD` is not set (default):

| Tier | Trust Score | Kinds Allowed | Daily Rate |
|------|-------------|---------------|------------|
| A    | r = 0       | Kind 1 only   | 1          |
| B    | 0 < r < 0.5 | Kind 1 only   | 1-100      |
| C    | r ≥ 0.5     | All kinds     | 10,000     |

In this mode, there is no distinct high tier - all pubkeys with `r ≥ midThreshold` get the maximum rate and no backfill privileges.

## Configuration

Configuration is loaded from environment variables in [`main.go`](main.go:33):

- `MID_THRESHOLD` (default: 0.5) - trust score above which all kinds are allowed
- `HIGH_THRESHOLD` (optional) - trust score above which backfill is free; if not set, there is no distinct high tier and all pubkeys with `r ≥ midThreshold` get maximum rate
- `URL_POLICY_ENABLED` (optional) - enables URL restriction for users below `MID_THRESHOLD`
- `RANK_QUEUE_IP_DAILY_LIMIT` (default: 250) - max rank refresh requests per day per IP group
- `RELATR_RELAY` (default: wss://relay.contextvm.org) - ContextVM relay URL for rank lookups
- `RELATR_PUBKEY` (default: 750682303c9f0ddad75941b49edc9d46e3ed306b9ee3335338a21a3e404c5fa3) - Relatr service pubkey
- `RELATR_SECRET_KEY` (optional) - Secret key for signing rank requests; auto-generated if not provided
- `DEBUG` (optional) - Enable verbose debug logging and periodic observability metrics

## Usage

### Environment Setup

Create a `.env` file or set environment variables:

```bash
# Optional (defaults shown)
export MID_THRESHOLD=0.5
# HIGH_THRESHOLD is optional - omit to use 3-tier system (no high tier)
# export HIGH_THRESHOLD=0.9  # uncomment to enable 4-tier system with backfill
# URL_POLICY_ENABLED is optional - enable to restrict URLs below MID_THRESHOLD
# export URL_POLICY_ENABLED="true"  # truthy: true/1/yes/on (case-insensitive)
export RANK_QUEUE_IP_DAILY_LIMIT=250
export RELATR_RELAY="wss://relay.contextvm.org"
export RELATR_PUBKEY="750682303c9f0ddad75941b49edc9d46e3ed306b9ee3335338a21a3e404c5fa3"
export RELATR_SECRET_KEY="your-secret-key-here"  # auto-generated if not set
export DEBUG="1"  # optional: enable verbose logging and metrics
```

### Building

```bash
go build
```

### Running

```bash
./wotrlay
```

The relay listens on `localhost:3334` by default.

## How It Works

1. **Event received**: Extract `event.PubKey`
2. **Rank lookup**: Query cache for trust score (cache miss → `r=0`)
3. **Kind check**: Reject non-Kind-1 if `r < MID_THRESHOLD`
4. **Timestamp check**: Reject events >24h in the future
5. **Backfill check**: Skip rate limiting if `HIGH_THRESHOLD` is set and `r ≥ HIGH_THRESHOLD` and event is old
6. **Rate limit**: Apply token bucket with trust-based refill rate
7. **Save**: Store event if all checks pass

## Architecture

- [`main.go`](main.go) - Relay setup and event handling
- [`rate.go`](rate.go) - Token bucket implementation
- [`rank.go`](rank.go) - Rank cache and refresh pipeline

## Operational Notes

### Error Handling

The relay returns typed errors for event rejections that can be used for client-side handling:

- `ErrKindNotAllowed` - Non-Kind-1 events from pubkeys below `MID_THRESHOLD`
- `ErrInvalidTimestamp` - Events with timestamps >24h in the future
- `ErrRateLimited` - Pubkey has exceeded their rate limit
- `ErrURLNotAllowed` - Kind 1 events with URLs from pubkeys below `MID_THRESHOLD` (only when `URL_POLICY_ENABLED=true`)

### Rank Cache Behavior

- **Cache hit**: Non-blocking lookup returns immediately
- **Cache miss**: Best-effort async refresh; event proceeds with rank=0
- **Stale data**: Entries older than `StaleThreshold` (24h) trigger async refresh
- **Deduplication**: Concurrent `GetRank` calls for the same pubkey are deduplicated to avoid duplicate network requests
- **Periodic flush**: The refresher flushes queued requests every `StaleThreshold` (24h) or when batch is full (1000 pubkeys)

### Rate Limiting

- **Token bucket**: Continuous refill (not daily reset) based on trust score
- **Capacity**: Minimum 1 token to ensure pubkeys can always publish eventually
- **TTL**: Inactive buckets are cleaned up after 1 hour
- **Monitoring**: `Limiter.GetTokens()` is available for debugging but should not be used in production code
- **Observability**: Built-in atomic counters track error types and cache behavior; logged periodically when DEBUG is enabled

### Security

- **Secret key**: `RELATR_SECRET_KEY` is never logged. If not provided, a temporary key is auto-generated (logged as "generated temporary key" without the value)
- **Configuration**: All sensitive values should be provided via environment variables

## Observability

When `DEBUG` is enabled, the relay logs operational metrics every 30 seconds:

```
observability: rate_limited=5 kind_not_allowed=2 invalid_timestamp=1 cache_hits=150 cache_misses=25
```

**Metrics tracked:**

- `rate_limited` - Number of events rejected due to rate limiting
- `kind_not_allowed` - Number of events rejected due to kind gating
- `invalid_timestamp` - Number of events rejected due to future timestamps
- `url_not_allowed` - Number of events rejected due to URL policy
- `cache_hits` - Number of rank cache hits
- `cache_misses` - Number of rank cache misses

**Usage:**

1. Enable debug mode: `export DEBUG=1`
2. Run the relay: `./wotrlay`
3. Watch logs for periodic metrics output

These metrics are useful for:
- Monitoring relay health and performance
- Detecting abuse patterns
- Tuning rate limit thresholds
- Understanding cache hit ratios

## License

MIT
