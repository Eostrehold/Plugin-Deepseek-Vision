// Package cache implements a bounded, TTL-aware in-memory cache. Only
// derived text is stored; image bytes are never retained here.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type entry struct {
	key       string
	value     string
	expiresAt time.Time
}

type Cache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*list.Element
	lru      *list.List
}

type Options struct {
	Capacity int
	TTL      time.Duration
}

func New(opts Options) *Cache {
	if opts.Capacity < 0 {
		opts.Capacity = 0
	}
	return &Cache{capacity: opts.Capacity, ttl: opts.TTL, items: make(map[string]*list.Element), lru: list.New()}
}

// NewLRU is a convenient constructor used by callers that do not need the
// options struct.
func NewLRU(capacity int, ttl time.Duration) *Cache {
	return New(Options{Capacity: capacity, TTL: ttl})
}

func (c *Cache) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return "", false
	}
	item := e.Value.(*entry)
	if c.ttl > 0 && !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		delete(c.items, key)
		c.lru.Remove(e)
		return "", false
	}
	c.lru.MoveToFront(e)
	return item.value, true
}

func (c *Cache) Set(key, value string) {
	if c == nil || c.capacity == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := time.Time{}
	if c.ttl > 0 {
		expires = time.Now().Add(c.ttl)
	}
	if e, ok := c.items[key]; ok {
		e.Value.(*entry).value = value
		e.Value.(*entry).expiresAt = expires
		c.lru.MoveToFront(e)
		return
	}
	e := c.lru.PushFront(&entry{key: key, value: value, expiresAt: expires})
	c.items[key] = e
	for len(c.items) > c.capacity {
		old := c.lru.Back()
		if old == nil {
			break
		}
		delete(c.items, old.Value.(*entry).key)
		c.lru.Remove(old)
	}
}

func (c *Cache) Delete(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		delete(c.items, key)
		c.lru.Remove(e)
	}
}

func (c *Cache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.lru.Init()
}

func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Key returns a stable, collision-resistant key from the operation inputs.
// Length prefixes avoid ambiguity such as ("ab", "c") vs ("a", "bc").
func Key(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte{byte(len(p) >> 24), byte(len(p) >> 16), byte(len(p) >> 8), byte(len(p))})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
