// Package main implements a Web-of-Trust (WoT) based Nostr relay
// with reputation-driven rate limiting. It enforces community spam-protection
// using external trust scores, with rate limits determined by a pubkey's reputation.
package main

import (
	"context"
	"sync"
	"time"
)

// Limiter manages token buckets for rate limiting.
// Buckets are automatically cleaned up based on TimeToLive.
type Limiter struct {
	mu      sync.RWMutex
	buckets map[string]*Bucket

	TimeToLive      time.Duration // How long to keep inactive buckets
	CleanupInterval time.Duration // How often to scan for cleanup
}

// Bucket represents a token bucket with continuous refill.
// Tokens are stored as float64 to support fractional accumulation.
type Bucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	refillRate float64   // tokens per second
	lastActive time.Time // last refill or consume time, used for TTL
}

func NewLimiter(ctx context.Context) *Limiter {
	limiter := &Limiter{
		buckets:         make(map[string]*Bucket, 100),
		TimeToLive:      time.Hour,
		CleanupInterval: time.Hour,
	}

	go limiter.cleaner(ctx)
	return limiter
}

// getOrCreateBucket returns an existing bucket or creates a new one with the specified parameters.
func (l *Limiter) getOrCreateBucket(id string, capacity, refillRate float64) *Bucket {
	l.mu.RLock()
	b, exists := l.buckets[id]
	l.mu.RUnlock()

	if exists {
		return b
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Double-check after acquiring write lock
	if b, exists = l.buckets[id]; !exists {
		b = &Bucket{
			tokens:     capacity, // Start full
			capacity:   capacity,
			refillRate: refillRate,
			lastActive: time.Now(),
		}
		l.buckets[id] = b
	}

	return b
}

// Allow checks if the bucket has at least 1 token and consumes it if so.
// This is a convenience wrapper for Consume with cost=1.
func (l *Limiter) Allow(id string, capacity, refillRate float64) bool {
	return l.Consume(id, 1, capacity, refillRate)
}

// Consume attempts to consume the specified cost from the bucket.
// Returns true if successful, false if insufficient tokens.
func (l *Limiter) Consume(id string, cost float64, capacity, refillRate float64) bool {
	b := l.getOrCreateBucket(id, capacity, refillRate)
	b.mu.Lock()
	defer b.mu.Unlock()

	// Update bucket parameters in case they changed (e.g., rank updated)
	b.capacity = capacity
	b.refillRate = refillRate

	// Refill tokens based on elapsed time
	b.refillLocked(time.Now())

	// Check if we have enough tokens
	if b.tokens < cost {
		return false
	}

	b.tokens -= cost
	return true
}

// GetTokens returns the current token count for a bucket (for debugging/monitoring).
// This method is intended for internal use and debugging purposes only.
func (l *Limiter) GetTokens(id string) float64 {
	l.mu.RLock()
	b, exists := l.buckets[id]
	l.mu.RUnlock()

	if !exists {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill before returning
	b.refillLocked(time.Now())
	return b.tokens
}

// refillLocked refills tokens based on elapsed time.
// Must be called with b.mu held.
func (b *Bucket) refillLocked(now time.Time) {
	elapsed := now.Sub(b.lastActive).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillRate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastActive = now
	}
}

// Clean scans through the buckets and removes the ones that are too old.
// Uses lastActive as the last activity timestamp for TTL calculation.
func (l *Limiter) Clean() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for id, b := range l.buckets {
		if now.Sub(b.lastActive) > l.TimeToLive {
			delete(l.buckets, id)
		}
	}
}

func (l *Limiter) cleaner(ctx context.Context) {
	timer := time.NewTicker(l.CleanupInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			l.Clean()
		}
	}
}
