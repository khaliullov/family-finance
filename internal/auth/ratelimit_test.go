package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRateLimiter_AllowsWithinLimitAndBlocksExcess(t *testing.T) {
	ctx := context.Background()
	limiter := NewBoundedMemoryRateLimiter(100)
	key := "test:client:1"
	limit := 3
	window := 100 * time.Millisecond

	// 1. First 3 requests should be allowed
	for i := 1; i <= limit; i++ {
		allowed, retryAfter := limiter.Allow(ctx, key, limit, window)
		if !allowed {
			t.Fatalf("request %d should have been allowed", i)
		}
		if retryAfter != 0 {
			t.Fatalf("retryAfter should be 0 for allowed request, got %v", retryAfter)
		}
	}

	// 2. 4th request must be blocked
	allowed, retryAfter := limiter.Allow(ctx, key, limit, window)
	if allowed {
		t.Fatalf("4th request should have been rate limited")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter, got %v", retryAfter)
	}

	// 3. Wait for sliding window to expire
	time.Sleep(window + 10*time.Millisecond)

	// 4. Request should be allowed again
	allowed, _ = limiter.Allow(ctx, key, limit, window)
	if !allowed {
		t.Fatalf("request after window expiration should be allowed")
	}
}

func TestRateLimiter_CapacityBoundingAndLRUEviction(t *testing.T) {
	ctx := context.Background()
	maxEntries := 5
	limiter := NewBoundedMemoryRateLimiter(maxEntries)

	// Add 5 distinct keys
	for i := 1; i <= maxEntries; i++ {
		key := fmt.Sprintf("key:%d", i)
		limiter.Allow(ctx, key, 10, time.Minute)
	}

	if limiter.evictList.Len() != maxEntries {
		t.Fatalf("expected %d entries, got %d", maxEntries, limiter.evictList.Len())
	}

	// Add 6th key -> should evict LRU key (key:1)
	limiter.Allow(ctx, "key:6", 10, time.Minute)

	if limiter.evictList.Len() != maxEntries {
		t.Fatalf("expected bounded %d entries after eviction, got %d", maxEntries, limiter.evictList.Len())
	}

	limiter.mu.Lock()
	_, key1Exists := limiter.items["key:1"]
	_, key6Exists := limiter.items["key:6"]
	limiter.mu.Unlock()

	if key1Exists {
		t.Fatalf("expected key:1 to be evicted as LRU")
	}
	if !key6Exists {
		t.Fatalf("expected key:6 to exist in cache")
	}
}

func TestRateLimiter_ConcurrentAccessThreadSafe(t *testing.T) {
	ctx := context.Background()
	limiter := NewBoundedMemoryRateLimiter(1000)
	key := "concurrent:test"
	limit := 50
	window := 1 * time.Second

	var wg sync.WaitGroup
	var allowedCount int
	var mu sync.Mutex

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _ := limiter.Allow(ctx, key, limit, window)
			if allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if allowedCount != limit {
		t.Fatalf("expected exactly %d allowed requests under concurrent contention, got %d", limit, allowedCount)
	}
}
