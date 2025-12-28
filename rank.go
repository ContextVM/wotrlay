// Package main implements a Web-of-Trust (WoT) based Nostr relay
// with reputation-driven rate limiting. It enforces community spam-protection
// using external trust scores, with rate limits determined by a pubkey's reputation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"golang.org/x/sync/singleflight"
)

// Pool for reducing GC pressure in refreshBatch.
var jsonRequestPool = sync.Pool{
	New: func() any {
		return &jsonRPCRequest{}
	},
}

type RankCache struct {
	mu      sync.RWMutex
	ranks   map[string]TimeRank
	refresh chan string

	StaleThreshold     time.Duration
	MaxRefreshInterval time.Duration

	// Configuration for rank lookups
	relatrRelay     string
	relatrPubkey    string
	relatrSecretKey string

	// Relay connection for reuse (reconnects on failure)
	relayMu sync.Mutex
	relay   *nostr.Relay

	// Single-flight group to prevent duplicate network requests
	flight singleflight.Group

	// lastClean tracks when the last eviction scan was performed
	lastClean time.Time

	// Observability metrics
	obs *Observability
}

type TimeRank struct {
	Timestamp time.Time
	Rank      float64
}

type PubRank struct {
	Pubkey string  `json:"pubkey"`
	Rank   float64 `json:"rank"`
}

// JSON-RPC request structures for ContextVM calculate_trust_scores
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type calculateTrustScoresParams struct {
	TargetPubkeys []string `json:"targetPubkeys"`
}

type toolCallParams struct {
	Name      string                      `json:"name"`
	Arguments *calculateTrustScoresParams `json:"arguments"`
}

// JSON-RPC response structures
type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			TrustScores []struct {
				TargetPubkey string  `json:"targetPubkey"`
				Score        float64 `json:"score"`
			} `json:"trustScores"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func NewRankCache(ctx context.Context, cfg Config, obs *Observability) *RankCache {
	cache := &RankCache{
		ranks:              make(map[string]TimeRank, 100),
		refresh:            make(chan string, 100),
		StaleThreshold:     24 * time.Hour,
		MaxRefreshInterval: 7 * 24 * time.Hour,
		relatrRelay:        cfg.RelatrRelay,
		relatrPubkey:       cfg.RelatrPubkey,
		relatrSecretKey:    cfg.RelatrSecretKey,
		lastClean:          time.Now(),
		obs:                obs,
	}

	go cache.refresher(ctx)
	return cache
}

// getRelay returns the cached relay connection, establishing one if needed.
// The connection is reused across requests and reconnected on failure.
func (c *RankCache) getRelay(ctx context.Context) (*nostr.Relay, error) {
	c.relayMu.Lock()
	defer c.relayMu.Unlock()

	if c.relay != nil && c.relay.IsConnected() {
		return c.relay, nil
	}

	// Close old connection if exists
	if c.relay != nil {
		c.relay.Close()
	}

	// Establish new connection
	newRelay, err := nostr.RelayConnect(ctx, c.relatrRelay)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", c.relatrRelay, err)
	}

	c.relay = newRelay
	return newRelay, nil
}

func (c *RankCache) dropRelay() {
	c.relayMu.Lock()
	defer c.relayMu.Unlock()
	if c.relay != nil {
		c.relay.Close()
		c.relay = nil
	}
}

// Rank returns the rank of the pubkey if it exists in the cache.
// If the rank is too old, its pubkey is sent to the refresher queue.
// This is a non-blocking call suitable for hot paths.
func (c *RankCache) Rank(pubkey string) (float64, bool) {
	c.mu.RLock()
	rank, exists := c.ranks[pubkey]
	c.mu.RUnlock()

	if !exists {
		c.obs.rankCacheMisses.Add(1)
		c.tryEnqueue(pubkey)
		return 0, false
	}

	if time.Since(rank.Timestamp) > c.StaleThreshold {
		c.tryEnqueue(pubkey)
	}
	c.obs.rankCacheHits.Add(1)
	return rank.Rank, true
}

// tryEnqueue attempts to enqueue a pubkey for refresh without blocking.
func (c *RankCache) tryEnqueue(pubkey string) {
	select {
	case c.refresh <- pubkey:
	default:
		// If refresh channel is full, skip to avoid blocking
	}
}

// GetRank returns the rank for a pubkey, blocking until the rank is available.
// If the rank is not in cache, it performs an immediate refresh request.
// This is suitable for scenarios where you need the rank result immediately.
// Uses singleflight to prevent duplicate network requests.
func (c *RankCache) GetRank(ctx context.Context, pubkey string) (float64, error) {
	// First check cache
	c.mu.RLock()
	rank, exists := c.ranks[pubkey]
	c.mu.RUnlock()

	if exists && time.Since(rank.Timestamp) <= c.StaleThreshold {
		return rank.Rank, nil
	}

	// Not in cache or stale, use singleflight to deduplicate
	_, err, _ := c.flight.Do(pubkey, func() (interface{}, error) {
		if err := c.refreshBatch(ctx, []string{pubkey}); err != nil {
			// On failure, cache rank=0 to avoid repeated lookups
			c.Update(time.Now(), PubRank{Pubkey: pubkey, Rank: 0})
			return nil, fmt.Errorf("failed to refresh rank: %w", err)
		}
		return nil, nil
	})

	if err != nil {
		return 0, err
	}
	// Check cache again after refresh
	c.mu.RLock()
	rank, exists = c.ranks[pubkey]
	c.mu.RUnlock()

	if !exists {
		// If still not in cache after successful refresh, cache as rank=0
		c.Update(time.Now(), PubRank{Pubkey: pubkey, Rank: 0})
		return 0, nil
	}

	return rank.Rank, nil
}

// Update uses the provided ranks to update the cache.
// Ranks are clamped to [0,1] to ensure valid values.
func (c *RankCache) Update(ts time.Time, ranks ...PubRank) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, r := range ranks {
		// Clamp rank to valid range [0,1]
		if r.Rank < 0 {
			r.Rank = 0
		} else if r.Rank > 1 {
			r.Rank = 1
		}
		c.ranks[r.Pubkey] = TimeRank{Rank: r.Rank, Timestamp: ts}
	}
}

// updateAndClean updates ranks and removes expired entries while holding the lock once.
// Eviction only runs if enough time has elapsed since the last clean (MaxRefreshInterval/2).
// Ranks are clamped to [0,1] to ensure valid values.
func (c *RankCache) updateAndClean(ts time.Time, ranks []PubRank) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, r := range ranks {
		// Clamp rank to valid range [0,1]
		if r.Rank < 0 {
			r.Rank = 0
		} else if r.Rank > 1 {
			r.Rank = 1
		}
		c.ranks[r.Pubkey] = TimeRank{Rank: r.Rank, Timestamp: ts}
	}

	// Only run eviction if enough time has elapsed since last clean
	now := time.Now()
	cleanInterval := c.MaxRefreshInterval / 2
	if now.Sub(c.lastClean) < cleanInterval {
		return
	}

	// Perform eviction scan
	for pk, rank := range c.ranks {
		if now.Sub(rank.Timestamp) > c.MaxRefreshInterval {
			delete(c.ranks, pk)
		}
	}
	c.lastClean = now
}

const MaxPubkeysToRank = 1000

// The cache refresher updates the ranks via the service provider and deletes
// old ranks. It fires when one of the following condition is met:
// - enough unique pubkeys need updated ranks
// - enough time has passed since the last refresh (based on StaleThreshold)
func (c *RankCache) refresher(ctx context.Context) {
	batch := make([]string, 0, MaxPubkeysToRank)
	seen := make(map[string]struct{}, MaxPubkeysToRank)
	ticker := time.NewTicker(c.StaleThreshold)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case pubkey, ok := <-c.refresh:
			if !ok {
				return
			}

			// Skip if already in current batch (O(1) lookup)
			if _, exists := seen[pubkey]; exists {
				continue
			}

			// Add to batch and mark as seen
			batch = append(batch, pubkey)
			seen[pubkey] = struct{}{}

			// Flush when batch is full
			if len(batch) >= MaxPubkeysToRank {
				if err := c.refreshBatch(ctx, batch); err != nil {
					log.Printf("failed to refresh cache: %v", err)
				}
				c.resetBatch(&batch, seen)
			}

		case <-ticker.C:
			// Periodic flush based on StaleThreshold
			if len(batch) > 0 {
				if err := c.refreshBatch(ctx, batch); err != nil {
					log.Printf("failed to refresh cache: %v", err)
				}
				c.resetBatch(&batch, seen)
			}
		}
	}
}

// resetBatch clears the batch slice and seen map without reallocating.
func (c *RankCache) resetBatch(batch *[]string, seen map[string]struct{}) {
	*batch = (*batch)[:0]
	for k := range seen {
		delete(seen, k)
	}
}

func (c *RankCache) refreshBatch(ctx context.Context, batch []string) error {
	if len(batch) < 1 {
		return nil
	}

	// Get request from pool and populate it
	req := jsonRequestPool.Get().(*jsonRPCRequest)
	defer jsonRequestPool.Put(req)

	*req = jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: toolCallParams{
			Name: "calculate_trust_scores",
			Arguments: &calculateTrustScoresParams{
				TargetPubkeys: batch,
			},
		},
	}

	contentBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON-RPC request: %w", err)
	}

	request := &nostr.Event{
		Kind:      25910,
		CreatedAt: nostr.Now(),
		Content:   string(contentBytes),
		Tags: nostr.Tags{
			nostr.Tag{"p", c.relatrPubkey},
		},
	}

	if err := request.Sign(c.relatrSecretKey); err != nil {
		return fmt.Errorf("failed to sign: %w", err)
	}

	response, err := c.contextVMResponse(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to get response: %w", err)
	}

	// Parse ContextVM response using typed struct
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(response.Content), &resp); err != nil {
		return fmt.Errorf("failed to unmarshal JSON-RPC response: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("JSON-RPC error: %s", resp.Error.Message)
	}

	if resp.Result.IsError {
		return fmt.Errorf("tool execution error")
	}

	// Convert to PubRank format
	ranks := make([]PubRank, 0, len(resp.Result.StructuredContent.TrustScores))
	for _, ts := range resp.Result.StructuredContent.TrustScores {
		ranks = append(ranks, PubRank{
			Pubkey: ts.TargetPubkey,
			Rank:   ts.Score,
		})
	}

	c.updateAndClean(response.CreatedAt.Time(), ranks)
	return nil
}

// contextVMResponse sends the request and fetches the response using the request ID.
// It reuses the cached relay connection for efficiency.
func (c *RankCache) contextVMResponse(ctx context.Context, request *nostr.Event) (*nostr.Event, error) {
	// Add timeout to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	relay, err := c.getRelay(ctx)
	if err != nil {
		return nil, err
	}

	if err := relay.Publish(ctx, *request); err != nil {
		// On publish error, close the connection to force reconnect next time.
		c.dropRelay()
		return nil, fmt.Errorf("failed to publish to %s: %v", c.relatrRelay, err)
	}

	// ContextVM uses same kind (25910) for both requests and responses
	// Responses are correlated using 'e' tags referencing the request ID
	filter := nostr.Filter{
		Kinds:   []int{25910},
		Tags:    nostr.TagMap{"e": {request.ID}},
		Authors: []string{c.relatrPubkey},
	}

	results, err := relay.QuerySync(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the response: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("failed to fetch the response: no responses received")
	}

	// If multiple responses, pick the first one and log a warning
	if len(results) > 1 {
		log.Printf("WARNING: received %d responses for request %s, using first one", len(results), request.ID)
	}

	return results[0], nil
}
