package auth

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// RateLimiter defines the interface for checking and enforcing rate limits.
// Can be backed by an in-memory sliding window cache or a shared production store (e.g., Redis).
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration)
}

// RateLimitConfig holds configurable thresholds for authentication and session endpoints.
type RateLimitConfig struct {
	LoginPerIPLimit       int           `json:"login_per_ip_limit"`
	LoginPerIPWindow      time.Duration `json:"login_per_ip_window"`
	LoginPerAccountLimit  int           `json:"login_per_account_limit"`
	LoginPerAccountWindow time.Duration `json:"login_per_account_window"`

	RegisterPerIPLimit  int           `json:"register_per_ip_limit"`
	RegisterPerIPWindow time.Duration `json:"register_per_ip_window"`

	RefreshPerIPLimit  int           `json:"refresh_per_ip_limit"`
	RefreshPerIPWindow time.Duration `json:"refresh_per_ip_window"`
	RefreshTokenLimit  int           `json:"refresh_token_limit"`
	RefreshTokenWindow time.Duration `json:"refresh_token_window"`

	MaxEntries int `json:"max_entries"` // Memory cap to prevent memory exhaustion attacks
}

// DefaultRateLimitConfig returns safe production default rate limit parameters.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		LoginPerIPLimit:       10, // 10 attempts per minute per IP
		LoginPerIPWindow:      1 * time.Minute,
		LoginPerAccountLimit:  5, // 5 attempts per minute per account
		LoginPerAccountWindow: 1 * time.Minute,

		RegisterPerIPLimit:  5, // 5 registrations per minute per IP
		RegisterPerIPWindow: 1 * time.Minute,

		RefreshPerIPLimit:  30, // 30 refresh calls per minute per IP
		RefreshPerIPWindow: 1 * time.Minute,
		RefreshTokenLimit:  10, // 10 rotations per minute per token chain
		RefreshTokenWindow: 1 * time.Minute,

		MaxEntries: 10000, // Caps memory consumption to ~2-3 MB
	}
}

type rateLimitEntry struct {
	key        string
	timestamps []time.Time
}

// BoundedMemoryRateLimiter implements a thread-safe sliding window log rate limiter
// with an LRU eviction policy to ensure bounded memory usage under high volume or spoofing attacks.
type BoundedMemoryRateLimiter struct {
	mu         sync.Mutex
	maxEntries int
	items      map[string]*list.Element
	evictList  *list.List
}

// NewBoundedMemoryRateLimiter creates a new BoundedMemoryRateLimiter with capacity maxEntries.
func NewBoundedMemoryRateLimiter(maxEntries int) *BoundedMemoryRateLimiter {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &BoundedMemoryRateLimiter{
		maxEntries: maxEntries,
		items:      make(map[string]*list.Element),
		evictList:  list.New(),
	}
}

// Allow checks if the given key is allowed to make a request within the sliding window.
func (l *BoundedMemoryRateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-window)

	elem, exists := l.items[key]
	if !exists {
		// Enforce capacity bound before creating new entry
		if l.evictList.Len() >= l.maxEntries {
			l.evictExpiredOrOldest(cutoff)
		}

		entry := &rateLimitEntry{
			key:        key,
			timestamps: []time.Time{now},
		}
		newElem := l.evictList.PushFront(entry)
		l.items[key] = newElem
		return true, 0
	}

	// Move accessed entry to front of LRU list
	l.evictList.MoveToFront(elem)
	entry := elem.Value.(*rateLimitEntry)

	// Filter out expired timestamps
	validIdx := 0
	for i, ts := range entry.timestamps {
		if ts.After(cutoff) {
			validIdx = i
			break
		}
		validIdx = i + 1
	}
	if validIdx > 0 {
		entry.timestamps = entry.timestamps[validIdx:]
	}

	if len(entry.timestamps) >= limit {
		// Calculate retryAfter until the oldest valid timestamp exits the window
		retryAfter := entry.timestamps[0].Add(window).Sub(now)
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		return false, retryAfter
	}

	entry.timestamps = append(entry.timestamps, now)
	return true, 0
}

func (l *BoundedMemoryRateLimiter) evictExpiredOrOldest(cutoff time.Time) {
	back := l.evictList.Back()
	if back == nil {
		return
	}
	entry := back.Value.(*rateLimitEntry)
	// Check if all timestamps in oldest entry are expired
	if len(entry.timestamps) == 0 || !entry.timestamps[len(entry.timestamps)-1].After(cutoff) {
		l.removeElement(back)
		return
	}

	// If none strictly expired, evict the LRU back element to guarantee maxEntries bound
	l.removeElement(back)
}

func (l *BoundedMemoryRateLimiter) removeElement(elem *list.Element) {
	l.evictList.Remove(elem)
	entry := elem.Value.(*rateLimitEntry)
	delete(l.items, entry.key)
}
