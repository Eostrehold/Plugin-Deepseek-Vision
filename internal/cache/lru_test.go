package cache

import (
	"testing"
	"time"
)

func TestLRUEvictionAndTTL(t *testing.T) {
	c := NewLRU(2, 10*time.Millisecond)
	c.Set("a", "A")
	c.Set("b", "B")
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing")
	}
	c.Set("c", "C")
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used item was not evicted")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired item returned")
	}
}

func TestDisabledCache(t *testing.T) {
	c := NewLRU(0, time.Minute)
	c.Set("a", "A")
	if _, ok := c.Get("a"); ok || c.Len() != 0 {
		t.Fatal("zero capacity cache retained item")
	}
}
