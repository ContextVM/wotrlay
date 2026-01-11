// Package main implements a Web-of-Trust (WoT) based Nostr relay
// with reputation-driven rate limiting. It enforces community spam-protection
// using external trust scores, with rate limits determined by a pubkey's reputation.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
	"github.com/pippellia-btc/rely"
)

// Build-time variables (set via -ldflags)
var (
	Version   string = "dev"
	Commit    string = "unknown"
	BuildTime string = "unknown"
)

// Config holds application configuration parameters.
type Config struct {
	// MidThreshold: trust score above which all kinds are allowed
	MidThreshold float64

	// HighThreshold: trust score above which backfill is free and max rate applies
	// If nil, there is no distinct high tier and high-threshold policies apply to all values exceeding midThreshold
	HighThreshold *float64

	// URLPolicyEnabled: whether to enforce URL restriction for users below MidThreshold
	URLPolicyEnabled bool

	// GlobalRankRefreshLimit: max rank refresh requests per second, relay-wide
	GlobalRankRefreshLimit float64

	// RankCacheSize: maximum number of entries in rank cache (default: 100000)
	RankCacheSize int

	// RelatrRelay: ContextVM relay URL for rank lookups
	RelatrRelay string

	// RelatrPubkey: Relatr service pubkey
	RelatrPubkey string

	// RelatrSecretKey: Secret key for signing rank requests (should be loaded from env)
	RelatrSecretKey string

	// Debug: whether to enable verbose debug logging
	Debug bool

	// NIP-11 Relay Information Document configuration
	RelayName        string
	RelayDescription string
	RelayPubKey      string
	RelayContact     string
	Software         string
	Version          string
}

// Timestamp sanity window: reject events >24h in the future
const timestampSanityWindow = 24 * time.Hour

// Backfill age threshold: events older than this may be free for high-trust pubkeys
const backfillAgeThreshold = 24 * time.Hour

// secondsPerDay is the number of seconds in a day for rate calculations
const secondsPerDay = 86400

// Sentinel errors for event rejection reasons.
// Error strings should not be capitalized or end with punctuation.
var (
	ErrKindNotAllowed   = errors.New("kind-not-allowed: just kind 1 events")
	ErrInvalidTimestamp = errors.New("invalid-timestamp: event timestamp is too far in the future")
	ErrRateLimited      = errors.New("rate-limited: please try again later")
	ErrURLNotAllowed    = errors.New("url-not-allowed: only text notes without URLs")
)

// exemptKinds are event kinds that bypass rate limiting and kind gating.
var exemptKinds = map[int]bool{
	0:     true,
	3:     true,
	10002: true,
	10040: true,
	30382: true,
}

// Observability tracks operational metrics for monitoring and debugging.
type Observability struct {
	rateLimitedCount      atomic.Uint64
	kindNotAllowedCount   atomic.Uint64
	invalidTimestampCount atomic.Uint64
	urlNotAllowedCount    atomic.Uint64
	rankCacheHits         atomic.Uint64
	rankCacheMisses       atomic.Uint64
}

// loadConfig loads configuration from environment variables with defaults and validation.
func loadConfig() Config {
	// Best-effort load of .env into process environment.
	// Without this, variables set in a local .env file won't be visible to os.Getenv
	// unless the process environment is populated externally (e.g. `export ...`).
	//
	// Note: ignore errors so production/container deployments that don't ship a .env
	// file keep working.
	_ = godotenv.Load()

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
		MidThreshold:           getEnvFloat("MID_THRESHOLD", 0.5),
		HighThreshold:          highThreshold,
		URLPolicyEnabled:       getEnvBool("URL_POLICY_ENABLED", false),
		GlobalRankRefreshLimit: getEnvFloat("GLOBAL_RANK_REFRESH_LIMIT", 500),
		RankCacheSize:          getEnvInt("RANK_CACHE_SIZE", 100000),
		RelatrRelay:            getEnvString("RELATR_RELAY", "wss://relay.contextvm.org"),
		RelatrPubkey:           getEnvString("RELATR_PUBKEY", "750682303c9f0ddad75941b49edc9d46e3ed306b9ee3335338a21a3e404c5fa3"),
		RelatrSecretKey:        os.Getenv("RELATR_SECRET_KEY"),
		Debug:                  os.Getenv("DEBUG") != "",
		// NIP-11 Relay Information Document configuration
		RelayName:        getEnvString("RELAY_NAME", "wotrlay"),
		RelayDescription: getEnvString("RELAY_DESCRIPTION", "A Web-of-Trust (WoT) based Nostr relay with reputation-driven rate limiting"),
		RelayPubKey:      getEnvString("RELAY_PUBKEY", ""),
		RelayContact:     getEnvString("RELAY_CONTACT", ""),
		Software:         getEnvString("SOFTWARE", "https://github.com/contextvm/wotrlay"),
		Version:          getEnvString("VERSION", "0.1.0"),
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

// getEnvFloat reads a float64 from environment variable with a default value.
func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
		log.Printf("Invalid value for %s: %s, using default: %f", key, value, defaultValue)
	}
	return defaultValue
}

// getEnvString reads a string from environment variable with a default value.
func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool reads a boolean from environment variable with a default value.
// Accepted true values: "true", "1", "yes", "on" (case-insensitive).
// Accepted false values: "false", "0", "no", "off" (case-insensitive).
// Any other non-empty value falls back to defaultValue.
func getEnvBool(key string, defaultValue bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}

	switch strings.ToLower(value) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		log.Printf("Invalid value for %s: %s, using default: %t", key, value, defaultValue)
		return defaultValue
	}
}

// getEnvInt reads an int from environment variable with a default value.
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
		log.Printf("Invalid value for %s: %s, using default: %d", key, value, defaultValue)
	}
	return defaultValue
}

// createRelayInfoDocument creates a NIP-11 compliant relay information document
// based on the configuration.
func createRelayInfoDocument(cfg Config) nip11.RelayInformationDocument {
	// Build supported NIPs list
	supportedNIPs := []any{1, 11} // Always support NIP-01 and NIP-11

	// Create the relay information document
	info := nip11.RelayInformationDocument{
		Name:          cfg.RelayName,
		Description:   cfg.RelayDescription,
		PubKey:        cfg.RelayPubKey,
		Contact:       cfg.RelayContact,
		SupportedNIPs: supportedNIPs,
		Software:      cfg.Software,
		Version:       cfg.Version,
	}

	return info
}

func main() {
	// Log version information
	log.Printf("Starting wotrlay relay v%s (commit: %s, built: %s)", Version, Commit, BuildTime)

	// Load configuration
	cfg := loadConfig()

	// Initialize observability metrics
	obs := &Observability{}

	// Setup context with proper signal handling (SIGINT and SIGTERM)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize dependencies with configuration
	cache := NewRankCache(ctx, cfg, obs)
	limiter := NewLimiter(ctx)

	// Initialize Badger event store backend
	db := badger.BadgerBackend{Path: "./badger"}
	if err := db.Init(); err != nil {
		log.Fatalf("failed to initialize badger backend: %v", err)
	}
	defer db.Close()

	// Start periodic observability logging if debug is enabled
	if cfg.Debug {
		go func() {
			ticker := time.NewTicker(30 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					logObservability(obs)
				}
			}
		}()
	}

	// Create NIP-11 relay information document
	relayInfo := createRelayInfoDocument(cfg)

	relay := rely.NewRelay(
		rely.WithDomain("relay.example.com"),
		rely.WithInfo(relayInfo),
	)

	// No NIP-42 auth requirement - rate limiting is based on event.PubKey
	relay.On.Event = func(c rely.Client, e *nostr.Event) error {
		return handleEvent(ctx, c, e, cfg, cache, limiter, &db, obs)
	}

	// Query hook for REQ messages
	relay.On.Req = func(ctx context.Context, c rely.Client, f nostr.Filters) ([]nostr.Event, error) {
		return Query(ctx, c, f, &db, cfg.Debug)
	}

	// Start the relay (non-blocking)
	relay.Start(ctx)

	// Create a custom handler that routes requests appropriately
	router := http.NewServeMux()

	// Serve favicon
	router.HandleFunc("/favicon.ico", serveFavicon())

	// Custom root handler that delegates to HTML or relay based on request type
	router.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route WebSocket and NIP-11 requests to the relay
		if r.Header.Get("Upgrade") == "websocket" || r.Header.Get("Accept") == "application/nostr+json" {
			relay.ServeHTTP(w, r)
			return
		}

		// For all other requests to root path, serve HTML
		if r.URL.Path == "/" && r.Method == http.MethodGet {
			serveHTMLPage(cfg, relayInfo)(w, r)
			return
		}

		// Let relay handle everything else
		relay.ServeHTTP(w, r)
	}))

	// Create HTTP server with custom router and proper timeouts.
	// Timeouts prevent resource exhaustion from slow clients.
	server := &http.Server{
		Addr:         "0.0.0.0:3334",
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	exitErr := make(chan error, 1)

	// Start the server
	go func() {
		log.Printf("Starting wotrlay relay on %s", server.Addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exitErr <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case <-ctx.Done():
		// Graceful shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		err := server.Shutdown(shutdownCtx)
		relay.Wait() // Wait for relay to close all connections
		if err != nil {
			log.Printf("Server shutdown error: %v", err)
		} else {
			log.Printf("Server shutdown complete")
		}

	case err := <-exitErr:
		log.Fatalf("Server error: %v", err)
	}
}

// handleEvent implements the v2 event handling flow.
func handleEvent(ctx context.Context, c rely.Client, e *nostr.Event, cfg Config, cache *RankCache, limiter *Limiter, db *badger.BadgerBackend, obs *Observability) error {
	now := time.Now()

	// 0. Exempt kinds bypass all rate limiting and kind gating
	if exemptKinds[e.Kind] {
		// Only timestamp sanity check applies to exempt kinds
		eventTime := time.Unix(int64(e.CreatedAt), 0)
		if eventTime.Sub(now) > timestampSanityWindow {
			obs.invalidTimestampCount.Add(1)
			return ErrInvalidTimestamp
		}
		// Save exempt kind events directly
		return Save(ctx, e, db, cfg.Debug)
	}

	// 1. Extract pubkey
	pubkey := e.PubKey

	// 2. Get rank from cache, with best-effort refresh on miss
	rank := lookupRank(ctx, c, e, cfg, cache, limiter, obs)

	// 3. Kind gating: only Kind 1 allowed below midThreshold
	if rank < cfg.MidThreshold && e.Kind != 1 {
		obs.kindNotAllowedCount.Add(1)
		return ErrKindNotAllowed
	}

	// 3.5. URL policy: no URLs allowed for users below mid threshold
	if cfg.URLPolicyEnabled && rank < cfg.MidThreshold && e.Kind == 1 && ContainsURL(e.Content) {
		obs.urlNotAllowedCount.Add(1)
		return ErrURLNotAllowed
	}

	// 4. Timestamp sanity: reject events too far in the future
	eventTime := time.Unix(int64(e.CreatedAt), 0)
	if eventTime.Sub(now) > timestampSanityWindow {
		obs.invalidTimestampCount.Add(1)
		return ErrInvalidTimestamp
	}

	// 5. Backfill rule: free for very high trust if event is old
	if cfg.HighThreshold != nil && rank >= *cfg.HighThreshold && now.Sub(eventTime) > backfillAgeThreshold {
		// Backfill is free - skip rate limiting
		return Save(ctx, e, db, cfg.Debug)
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
		obs.rateLimitedCount.Add(1)
		return ErrRateLimited
	}

	// 7. Save event
	return Save(ctx, e, db, cfg.Debug)
}

// calculateDailyRate returns the target allowed events per day based on trust score.
func calculateDailyRate(r float64, cfg Config) float64 {
	switch {
	case r <= 0:
		return 1
	case r < cfg.MidThreshold:
		// Tier B: linear 1 → 100
		return 1 + (r/cfg.MidThreshold)*99
	case cfg.HighThreshold != nil && r < *cfg.HighThreshold:
		// Tier C: linear 100 → 5000
		span := *cfg.HighThreshold - cfg.MidThreshold
		return 100 + ((r-cfg.MidThreshold)/span)*4900
	default:
		// Tier D: max rate
		return 10000
	}
}

// lookupRank returns the rank for a pubkey, performing a best-effort refresh on cache miss.
// Uses a global relay-wide limiter to protect rank provider from abuse.
// Preserves stale cache data when refresh fails or global limit is hit.
func lookupRank(ctx context.Context, c rely.Client, e *nostr.Event, cfg Config, cache *RankCache, limiter *Limiter, obs *Observability) float64 {
	pubkey := e.PubKey

	// Try cache first
	rank, exists := cache.Rank(pubkey)
	if exists {
		return rank
	}

	// Gate refresh attempts by global relay-wide limiter to protect rank provider from abuse
	if limiter.Allow("global-rank-refresh", cfg.GlobalRankRefreshLimit, cfg.GlobalRankRefreshLimit) {
		refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if refreshed, err := cache.GetRank(refreshCtx, pubkey); err == nil {
			return refreshed
		}
		// Refresh failed - check if we have stale data preserved
		if rank, exists := cache.Rank(pubkey); exists {
			if cfg.Debug {
				log.Printf("using stale rank %f for %s (refresh failed)", rank, pubkey)
			}
			return rank
		}
		// No stale data, enqueue for async refresh and proceed with rank=0
		cache.tryEnqueue(pubkey)
	} else {
		// Global rate-limited - check if we have stale data preserved
		if rank, exists := cache.Rank(pubkey); exists {
			if cfg.Debug {
				log.Printf("global rank refresh rate-limited, using stale rank %f for %s", rank, pubkey)
			}
			return rank
		}
		if cfg.Debug {
			log.Printf("global rank refresh rate-limited, no stale data available for %s", pubkey)
		}
	}
	return 0
}

func Save(ctx context.Context, e *nostr.Event, db *badger.BadgerBackend, debug bool) error {
	// Save event to Badger backend
	err := db.SaveEvent(ctx, e)
	if err != nil {
		log.Printf("failed to save event %s: %v", e.ID, err)
		return err
	}

	// Only log if DEBUG is enabled to reduce production noise
	if debug {
		log.Printf("saved event id=%s kind=%d pubkey=%s", e.ID, e.Kind, e.PubKey)
	}
	return nil
}

// Query handles REQ messages by querying the event store.
func Query(ctx context.Context, c rely.Client, f nostr.Filters, db *badger.BadgerBackend, debug bool) ([]nostr.Event, error) {
	if debug {
		log.Printf("received filters %v", f)
	}

	// Preallocate slice to reduce growth churn (128 is a reasonable default for most queries)
	events := make([]nostr.Event, 0, 128)

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

	if debug {
		log.Printf("query returned %d events", len(events))
	}
	return events, nil
}

// logObservability prints current counter values for debugging/monitoring.
func logObservability(obs *Observability) {
	// Load atomically to avoid race conditions
	rateLimited := obs.rateLimitedCount.Load()
	kindNotAllowed := obs.kindNotAllowedCount.Load()
	invalidTimestamp := obs.invalidTimestampCount.Load()
	urlNotAllowed := obs.urlNotAllowedCount.Load()
	cacheHits := obs.rankCacheHits.Load()
	cacheMisses := obs.rankCacheMisses.Load()

	log.Printf("observability: rate_limited=%d kind_not_allowed=%d invalid_timestamp=%d url_not_allowed=%d cache_hits=%d cache_misses=%d",
		rateLimited, kindNotAllowed, invalidTimestamp, urlNotAllowed, cacheHits, cacheMisses)
}
