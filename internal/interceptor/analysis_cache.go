package interceptor

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

type analysisCacheEntry struct {
	key       string
	value     string
	expiresAt time.Time
}

// analysisCache is intentionally small and generation-local. It stores only a
// hash key and derived text; image references and bytes are never retained.
type analysisCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	lru      *list.List
}

type idempotencyCacheEntry struct {
	key       string
	identity  string
	value     string
	err       error
	expiresAt time.Time
	pending   bool
	ready     chan struct{}
}

type idempotencyReservation struct {
	cache *idempotencyCache
	entry *idempotencyCacheEntry
}

// idempotencyCache binds a hashed agent call ID to the content-derived
// analysis identity. Neither call IDs nor image references are retained.
type idempotencyCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	lru      *list.List
}

var (
	errReanalysisCallConflict        = errors.New("reanalysis call ID was reused with different inputs")
	errIdempotencyReservationAborted = errors.New("idempotency reservation aborted before analysis")
)

func newAnalysisCache(capacity int) *analysisCache {
	if capacity < 0 {
		capacity = 0
	}
	return &analysisCache{capacity: capacity, items: make(map[string]*list.Element), lru: list.New()}
}

func (c *analysisCache) Get(key string) (string, bool) {
	if c == nil || c.capacity == 0 || key == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*analysisCacheEntry)
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		delete(c.items, key)
		c.lru.Remove(element)
		return "", false
	}
	c.lru.MoveToFront(element)
	return entry.value, true
}

func (c *analysisCache) Set(key, value string, ttl time.Duration) {
	if c == nil || c.capacity == 0 || key == "" || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt := time.Now().Add(ttl)
	if element, ok := c.items[key]; ok {
		entry := element.Value.(*analysisCacheEntry)
		entry.value = value
		entry.expiresAt = expiresAt
		c.lru.MoveToFront(element)
		return
	}
	element := c.lru.PushFront(&analysisCacheEntry{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = element
	for len(c.items) > c.capacity {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		delete(c.items, oldest.Value.(*analysisCacheEntry).key)
		c.lru.Remove(oldest)
	}
}

func (c *analysisCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func newIdempotencyCache(capacity int) *idempotencyCache {
	if capacity < 0 {
		capacity = 0
	}
	return &idempotencyCache{capacity: capacity, items: make(map[string]*list.Element), lru: list.New()}
}

// Reserve atomically claims a call ID/identity pair. The owner receives a
// reservation; identical concurrent callers wait for and reuse its outcome.
// Reusing a live call ID with a different identity fails immediately.
func (c *idempotencyCache) Reserve(ctx context.Context, callID, identity string) (*idempotencyReservation, string, bool, error) {
	if c == nil || c.capacity == 0 || callID == "" || identity == "" {
		return nil, "", false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := digestString(callID)
	for {
		c.mu.Lock()
		element, ok := c.items[key]
		if ok {
			entry := element.Value.(*idempotencyCacheEntry)
			if !entry.pending && !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
				delete(c.items, key)
				c.lru.Remove(element)
				ok = false
			} else if entry.identity != identity {
				c.mu.Unlock()
				return nil, "", false, errReanalysisCallConflict
			} else if entry.pending {
				ready := entry.ready
				c.mu.Unlock()
				select {
				case <-ready:
					if errors.Is(entry.err, errIdempotencyReservationAborted) {
						continue
					}
					if entry.err != nil {
						return nil, "", true, entry.err
					}
					return nil, entry.value, true, nil
				case <-ctx.Done():
					return nil, "", true, ctx.Err()
				}
			} else {
				value := entry.value
				c.lru.MoveToFront(element)
				c.mu.Unlock()
				return nil, value, true, nil
			}
		}
		if !ok {
			entry := &idempotencyCacheEntry{key: key, identity: identity, pending: true, ready: make(chan struct{})}
			element := c.lru.PushFront(entry)
			c.items[key] = element
			c.evictCompletedLocked()
			reservation := &idempotencyReservation{cache: c, entry: entry}
			c.mu.Unlock()
			return reservation, "", false, nil
		}
		c.mu.Unlock()
	}
}

// Complete publishes an owner's result. Failures wake identical waiters but
// are removed immediately so a later request may retry.
func (c *idempotencyCache) Complete(reservation *idempotencyReservation, value string, resultErr error, ttl time.Duration) {
	if c == nil || reservation == nil || reservation.cache != c || reservation.entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := reservation.entry
	element, ok := c.items[entry.key]
	if !ok || element.Value.(*idempotencyCacheEntry) != entry || !entry.pending {
		reservation.entry = nil
		return
	}
	entry.value = value
	entry.err = resultErr
	entry.pending = false
	if resultErr == nil && ttl > 0 {
		entry.expiresAt = time.Now().Add(ttl)
		c.lru.MoveToFront(element)
	} else {
		delete(c.items, entry.key)
		c.lru.Remove(element)
	}
	close(entry.ready)
	reservation.entry = nil
	c.evictCompletedLocked()
}

func (c *idempotencyCache) evictCompletedLocked() {
	for len(c.items) > c.capacity {
		var candidate *list.Element
		for element := c.lru.Back(); element != nil; element = element.Prev() {
			if !element.Value.(*idempotencyCacheEntry).pending {
				candidate = element
				break
			}
		}
		if candidate == nil {
			return
		}
		delete(c.items, candidate.Value.(*idempotencyCacheEntry).key)
		c.lru.Remove(candidate)
	}
}

func analysisGroupCacheKey(images []vision.ImageInput, models []string, language, prompt string, dataTTL, urlTTL time.Duration) (string, time.Duration) {
	ttl := dataTTL
	parts := []string{"prompt-group-v2", vision.NormalizeLanguage(language), vision.BuildPrompt(prompt, language)}
	parts = append(parts, models...)
	for i := range images {
		identity, isData := imageReferenceIdentity(images[i].Reference)
		parts = append(parts, identity)
		if !isData {
			ttl = urlTTL
		}
	}
	hash := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil)), ttl
}

func imageReferenceIdentity(reference string) (string, bool) {
	trimmed := strings.TrimSpace(reference)
	if len(trimmed) >= len("data:") && strings.EqualFold(trimmed[:len("data:")], "data:") {
		comma := strings.IndexByte(trimmed, ',')
		if comma > 0 {
			payload, err := url.PathUnescape(trimmed[comma+1:])
			if err == nil {
				var decoded []byte
				header := strings.ToLower(trimmed[:comma])
				if strings.HasSuffix(header, ";base64") {
					decoded, err = base64.StdEncoding.Strict().DecodeString(payload)
					if err != nil {
						decoded, err = base64.RawStdEncoding.Strict().DecodeString(payload)
					}
				} else {
					decoded = []byte(payload)
				}
				if err == nil {
					return digestBytes(decoded), true
				}
			}
		}
		return digestString(trimmed), true
	}
	if parsed, err := url.ParseRequestURI(trimmed); err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		trimmed = parsed.String()
	}
	return digestString(trimmed), false
}

func digestString(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
