package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
)

// Config holds application configuration parameters.
type Config struct {
	// MidThreshold: trust score above which all kinds are allowed
	MidThreshold float64

	// HighThreshold: trust score above which backfill is free and max rate applies
	// If nil, there is no distinct high tier and high-threshold policies apply to all values exceeding midThreshold
	HighThreshold *float64

	// RankQueueIPDailyLimit: max rank refresh requests per day per IP group
	RankQueueIPDailyLimit float64

	// RelatrRelay: ContextVM relay URL for rank lookups
	RelatrRelay string

	// RelatrPubkey: Relatr service pubkey
	RelatrPubkey string

	// RelatrSecretKey: Secret key for signing rank requests (should be loaded from env)
	RelatrSecretKey string
}

// Timestamp sanity window: reject events >24h in the future
const timestampSanityWindow = 24 * time.Hour

// Backfill age threshold: events older than this may be free for high-trust pubkeys
const backfillAgeThreshold = 24 * time.Hour

// secondsPerDay is the number of seconds in a day for rate calculations
const secondsPerDay = 86400

// rankQueueKeyPrefix is the prefix for rank-queue rate limiter keys
const rankQueueKeyPrefix = "rank-queue:"

// Sentinel errors for event rejection reasons
var (
	ErrKindNotAllowed   = errors.New("kind-not-allowed: just Kind 1 events")
	ErrInvalidTimestamp = errors.New("invalid-timestamp: event timestamp is too far in the future")
	ErrRateLimited      = errors.New("rate-limited: please try again later")
)

// loadConfig loads configuration from environment variables with defaults and validation.
func loadConfig() Config {
	// Get HighThreshold as optional parameter
	var highThreshold *float64
	if value := os.Getenv("HIGH_THRESHOLD"); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			highThreshold = &parsed
		} else {
			log.Printf("Invalid value for HIGH_THRESHOLD: %s, treating as unset", value)
		}
	}

	cfg := Config{
		MidThreshold:          getEnvFloat("MID_THRESHOLD", 0.5),
		HighThreshold:         highThreshold,
		RankQueueIPDailyLimit: getEnvFloat("RANK_QUEUE_IP_DAILY_LIMIT", 100),
		RelatrRelay:           getEnvString("RELATR_RELAY", "wss://relay.contextvm.org"),
		RelatrPubkey:          getEnvString("RELATR_PUBKEY", "750682303c9f0ddad75941b49edc9d46e3ed306b9ee3335338a21a3e404c5fa3"),
		RelatrSecretKey:       os.Getenv("RELATR_SECRET_KEY"),
	}

	// Validate thresholds
	if cfg.MidThreshold < 0 || cfg.MidThreshold > 1 {
		log.Fatal("MID_THRESHOLD must be between 0 and 1")
	}
	if cfg.HighThreshold != nil {
		if *cfg.HighThreshold < 0 || *cfg.HighThreshold > 1 {
			log.Fatal("HIGH_THRESHOLD must be between 0 and 1")
		}
		if *cfg.HighThreshold <= cfg.MidThreshold {
			log.Fatal("HIGH_THRESHOLD must be greater than MID_THRESHOLD")
		}
	}

	// Generate secret key if not provided
	if cfg.RelatrSecretKey == "" {
		cfg.RelatrSecretKey = nostr.GeneratePrivateKey()
		log.Printf("RELATR_SECRET_KEY not set, generated temporary key for this session")
	}

	return cfg
}

// getEnvFloat reads a float64 from environment variable with a default value
func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
		log.Printf("Invalid value for %s: %s, using default: %f", key, value, defaultValue)
	}
	return defaultValue
}

// getEnvString reads a string from environment variable with a default value
func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	// Load configuration
	cfg := loadConfig()

	// Setup context with proper signal handling (SIGINT and SIGTERM)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize dependencies with configuration
	cache := NewRankCache(ctx, cfg)
	limiter := NewLimiter(ctx)

	// Initialize Badger event store backend
	db := badger.BadgerBackend{Path: "./badger"}
	if err := db.Init(); err != nil {
		log.Fatalf("failed to initialize badger backend: %v", err)
	}
	defer db.Close()

	relay := rely.NewRelay(
		rely.WithDomain("relay.example.com"),
	)

	// No NIP-42 auth requirement - rate limiting is based on event.PubKey
	relay.On.Event = func(c rely.Client, e *nostr.Event) error {
		return handleEvent(ctx, c, e, cfg, cache, limiter, &db)
	}

	// Query hook for REQ messages
	relay.On.Req = func(ctx context.Context, c rely.Client, f nostr.Filters) ([]nostr.Event, error) {
		return Query(ctx, c, f, &db)
	}

	if err := relay.StartAndServe(ctx, "localhost:3334"); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// handleEvent implements the v2 event handling flow
func handleEvent(ctx context.Context, c rely.Client, e *nostr.Event, cfg Config, cache *RankCache, limiter *Limiter, db *badger.BadgerBackend) error {
	now := time.Now()

	// 1. Extract pubkey
	pubkey := e.PubKey

	// 2. Get rank from cache, with best-effort refresh on miss
	rank := lookupRank(ctx, c, e, cfg, cache, limiter)

	// 3. Kind gating: only Kind 1 allowed below midThreshold
	if rank < cfg.MidThreshold && e.Kind != 1 {
		return ErrKindNotAllowed
	}

	// 4. Timestamp sanity: reject events too far in the future
	eventTime := time.Unix(int64(e.CreatedAt), 0)
	if eventTime.Sub(now) > timestampSanityWindow {
		return ErrInvalidTimestamp
	}

	// 5. Backfill rule: free for very high trust if event is old
	if cfg.HighThreshold != nil && rank >= *cfg.HighThreshold && now.Sub(eventTime) > backfillAgeThreshold {
		// Backfill is free - skip rate limiting
		return Save(ctx, e, db)
	}

	// 6. Apply pubkey token bucket
	dailyRate := calculateDailyRate(rank, cfg)
	refillRate := dailyRate / secondsPerDay // tokens per second
	capacity := dailyRate / 24.0            // 1 hour worth of tokens
	// Each event costs 1 token. If capacity < 1, the bucket can never reach 1 token,
	// which would permanently rate-limit that pubkey.
	if capacity < 1 {
		capacity = 1
	}

	if !limiter.Allow(pubkey, capacity, refillRate) {
		return ErrRateLimited
	}

	// 7. Save event
	return Save(ctx, e, db)
}

// calculateDailyRate returns the target allowed events per day based on trust score
func calculateDailyRate(r float64, cfg Config) float64 {
	// Tier A: r == 0 - Kind 1 only, 1 event/day
	if r == 0 {
		return 1
	}

	// Tier B: 0 < r < midThreshold - Kind 1 only, linear from 1 to 100/day
	if r < cfg.MidThreshold {
		return 1 + (r/cfg.MidThreshold)*(100-1)
	}

	// Tier C: midThreshold ≤ r < highThreshold (if highThreshold is set) - all kinds, linear from 100 to 5000/day
	if cfg.HighThreshold != nil && r < *cfg.HighThreshold {
		return 100 + ((r-cfg.MidThreshold)/(*cfg.HighThreshold-cfg.MidThreshold))*(5000-100)
	}

	// Tier D: r ≥ highThreshold (if set) OR r ≥ midThreshold (if highThreshold is nil) - all kinds, cap at 10,000/day
	return 10000
}

// lookupRank returns the rank for a pubkey, performing a best-effort refresh on cache miss.
// It gates refresh attempts by IP to avoid a single IP forcing lots of rank lookups.
func lookupRank(ctx context.Context, c rely.Client, e *nostr.Event, cfg Config, cache *RankCache, limiter *Limiter) float64 {
	pubkey := e.PubKey
	rank, exists := cache.Rank(pubkey)
	if exists {
		return rank
	}

	// Gate refresh attempts by IP
	ipGroup := c.IP().Group()
	if limiter.Allow(rankQueueKeyPrefix+ipGroup, cfg.RankQueueIPDailyLimit, cfg.RankQueueIPDailyLimit/secondsPerDay) {
		refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if refreshed, err := cache.GetRank(refreshCtx, pubkey); err == nil {
			return refreshed
		}
		// Best-effort fallback: enqueue for async refresh and proceed with rank=0
		cache.tryEnqueue(pubkey)
	} else {
		log.Printf("rank-queue rate-limited for IP %s, skipping refresh for %s", ipGroup, pubkey)
	}
	return 0
}

func Save(ctx context.Context, e *nostr.Event, db *badger.BadgerBackend) error {
	// Save event to Badger backend
	err := db.SaveEvent(ctx, e)
	if err != nil {
		log.Printf("failed to save event %s: %v", e.ID, err)
		return err
	}

	// Minimal logging: only essential fields to avoid expensive event serialization
	log.Printf("saved event id=%s kind=%d pubkey=%s", e.ID, e.Kind, e.PubKey)
	return nil
}

// Query handles REQ messages by querying the event store
func Query(ctx context.Context, c rely.Client, f nostr.Filters, db *badger.BadgerBackend) ([]nostr.Event, error) {
	log.Printf("received filters %v", f)

	var events []nostr.Event

	// Query events from the Badger backend for each filter
	// The eventstore QueryEvents takes a single filter and returns a channel

	for _, filter := range f {
		eventChan, err := db.QueryEvents(ctx, filter)
		if err != nil {
			log.Printf("failed to query events with filter %v: %v", filter, err)
			continue
		}

		for event := range eventChan {
			events = append(events, *event)
		}
	}

	log.Printf("query returned %d events", len(events))
	return events, nil
}
