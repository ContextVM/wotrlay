package main

import (
	"context"
	"testing"
	"time"
)

func TestRankCacheIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Load test configuration (will auto-generate secret key if not set)
	cfg := loadConfig()

	// Create rank cache with configuration and observability
	obs := &Observability{}
	cache := NewRankCache(ctx, cfg, obs)

	// Test pubkey from the successful integration test
	testPubkey := "6b3780ef2972e73d370b84a3e51e7aa9ae34bf412938dcfbd9c5f63b221416c8"

	// Test blocking GetRank for cache miss
	t.Log("Testing blocking GetRank for cache miss...")
	rank, err := cache.GetRank(ctx, testPubkey)
	if err != nil {
		t.Errorf("GetRank failed: %v", err)
		return
	}

	t.Logf("✓ Rank received: %.4f", rank)
	if rank < 0 || rank > 1 {
		t.Errorf("Rank out of valid range [0,1]: %.4f", rank)
	}

	// Test that subsequent calls use cache
	t.Log("Testing cache hit...")
	rank2, err := cache.GetRank(ctx, testPubkey)
	if err != nil {
		t.Errorf("GetRank failed on cache hit: %v", err)
		return
	}

	if rank2 != rank {
		t.Errorf("Cache returned different rank: got %.4f, want %.4f", rank2, rank)
	} else {
		t.Logf("✓ Cache hit returned same rank: %.4f", rank2)
	}

	// Test non-blocking Rank method
	t.Log("Testing non-blocking Rank method...")
	r, found := cache.Rank(testPubkey)
	if !found {
		t.Error("Rank method should find cached rank")
	} else if r != rank {
		t.Errorf("Rank method returned different rank: got %.4f, want %.4f", r, rank)
	} else {
		t.Logf("✓ Rank method returned cached rank: %.4f", r)
	}
}
