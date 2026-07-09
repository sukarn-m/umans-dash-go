package proxy

import (
	"container/list"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ─── ImageHandoffCache (§11) ────────────────────────────────────────────────

// ─── ImageHandoffCache (§11) ────────────────────────────────────────────────

// NewImageHandoffCache creates a new LRU cache for vision handoff descriptions.
func NewImageHandoffCache(maxSize int, ttl time.Duration) *ImageHandoffCache {
	return &ImageHandoffCache{
		maxSize: maxSize,
		ttl:     ttl,
		lru:     list.New(),
		lookup:  make(map[string]*list.Element),
	}
}

// Get returns a cached description if it exists and hasn't expired.
func (c *ImageHandoffCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.lookup[key]
	if !ok {
		c.misses++
		return "", false
	}
	entry := elem.Value.(*handoffCacheEntry)
	if time.Since(entry.time) > c.ttl {
		c.lru.Remove(elem)
		delete(c.lookup, key)
		c.misses++
		return "", false
	}
	// LRU: move to front
	c.lru.MoveToFront(elem)
	c.hits++
	return entry.desc, true
}

// Set stores a description in the cache, evicting the oldest entry if at capacity.
func (c *ImageHandoffCache) Set(key, desc string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.lookup[key]; ok {
		entry := elem.Value.(*handoffCacheEntry)
		entry.desc = desc
		entry.time = time.Now()
		c.lru.MoveToFront(elem)
		return
	}
	entry := &handoffCacheEntry{hash: key, desc: desc, time: time.Now()}
	elem := c.lru.PushFront(entry)
	c.lookup[key] = elem
	if c.lru.Len() > c.maxSize {
		back := c.lru.Back()
		if back != nil {
			c.lru.Remove(back)
			delete(c.lookup, back.Value.(*handoffCacheEntry).hash)
			c.evictions++
		}
	}
}

// Stats returns current cache statistics.
func (c *ImageHandoffCache) Stats() HandoffCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return HandoffCacheStats{
		Size:      c.lru.Len(),
		MaxSize:   c.maxSize,
		TtlMs:     c.ttl.Milliseconds(),
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
	}
}

// Resize updates the cache's max size and TTL.
func (c *ImageHandoffCache) Resize(maxSize int, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxSize = maxSize
	c.ttl = ttl
}

// sha256Hash returns the hex-encoded SHA-256 hash of the input.
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// md5Hash returns the hex-encoded MD5 hash of the input.
// Used for conversation fingerprinting (§14) and usage history cache keys.
func md5Hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
