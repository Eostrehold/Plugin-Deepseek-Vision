package interceptor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

func TestAnalysisCacheKeyUsesContentAndPromptWithoutRetainingReference(t *testing.T) {
	reference := "data:image/png;base64,AAAA"
	dataTTL := 7 * time.Minute
	urlTTL := 30 * time.Second
	images := []vision.ImageInput{{Number: 1, Reference: reference}}
	first, firstTTL := analysisGroupCacheKey(images, []string{"vision-model"}, "zh-CN", "focus", dataTTL, urlTTL)
	second, secondTTL := analysisGroupCacheKey(images, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if first != second || firstTTL != dataTTL || secondTTL != dataTTL {
		t.Fatalf("equivalent keys differ: %q/%s %q/%s", first, firstTTL, second, secondTTL)
	}
	if strings.Contains(first, reference) || len(first) != 64 {
		t.Fatalf("cache key retained reference or is not SHA-256: %q", first)
	}
	changedFocus, _ := analysisGroupCacheKey(images, []string{"vision-model"}, "zh", "other", dataTTL, urlTTL)
	changedModel, _ := analysisGroupCacheKey(images, []string{"other-model"}, "zh", "focus", dataTTL, urlTTL)
	changedChain, _ := analysisGroupCacheKey(images, []string{"vision-model", "fallback"}, "zh", "focus", dataTTL, urlTTL)
	reorderedChain, _ := analysisGroupCacheKey(images, []string{"fallback", "vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if changedFocus == first || changedModel == first || changedChain == first || reorderedChain == changedChain {
		t.Fatal("cache key ignored prompt or model")
	}
	equivalentData := []vision.ImageInput{{Number: 1, Reference: "data:image/other;base64,AAAA"}}
	equivalentDataKey, _ := analysisGroupCacheKey(equivalentData, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if equivalentDataKey != first {
		t.Fatal("data URI identity did not use decoded image bytes")
	}
	ordered := []vision.ImageInput{{Number: 1, Reference: reference}, {Number: 2, Reference: "https://example.com/second.png"}}
	reversed := []vision.ImageInput{{Number: 2, Reference: "https://example.com/second.png"}, {Number: 1, Reference: reference}}
	orderedKey, _ := analysisGroupCacheKey(ordered, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	reversedKey, _ := analysisGroupCacheKey(reversed, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if orderedKey == reversedKey {
		t.Fatal("cache key ignored image order")
	}
	shifted := []vision.ImageInput{{Number: 11, Reference: reference}, {Number: 12, Reference: "https://example.com/second.png"}}
	shiftedKey, _ := analysisGroupCacheKey(shifted, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if shiftedKey != orderedKey {
		t.Fatal("cache key incorrectly depends on traversal-global image numbers")
	}
	_, gotURLTTL := analysisGroupCacheKey([]vision.ImageInput{{Number: 1, Reference: "https://example.com/image.png"}}, []string{"vision-model"}, "zh", "focus", dataTTL, urlTTL)
	if gotURLTTL != urlTTL || gotURLTTL >= dataTTL {
		t.Fatalf("URL TTL = %s, data TTL = %s", gotURLTTL, dataTTL)
	}
}

func TestIdempotencyCacheReservesAtomically(t *testing.T) {
	cache := newIdempotencyCache(4)
	owner, value, hit, err := cache.Reserve(context.Background(), "call_1", "identity_a")
	if err != nil || owner == nil || hit || value != "" {
		t.Fatalf("owner=%#v value=%q hit=%v err=%v", owner, value, hit, err)
	}
	if _, _, _, err := cache.Reserve(context.Background(), "call_1", "identity_b"); !errors.Is(err, errReanalysisCallConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	type result struct {
		value string
		hit   bool
		err   error
	}
	waiter := make(chan result, 1)
	go func() {
		_, value, hit, err := cache.Reserve(context.Background(), "call_1", "identity_a")
		waiter <- result{value: value, hit: hit, err: err}
	}()
	select {
	case got := <-waiter:
		t.Fatalf("identical waiter returned before completion: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}
	cache.Complete(owner, "analysis", nil, time.Minute)
	select {
	case got := <-waiter:
		if got.err != nil || !got.hit || got.value != "analysis" {
			t.Fatalf("waiter=%#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("identical waiter did not wake")
	}
}

func TestAnalysisCacheExpiresAndEvicts(t *testing.T) {
	cache := newAnalysisCache(2)
	cache.Set("one", "1", time.Minute)
	cache.Set("two", "2", time.Minute)
	if value, ok := cache.Get("one"); !ok || value != "1" {
		t.Fatalf("cache get = %q, %v", value, ok)
	}
	cache.Set("three", "3", time.Minute)
	if _, ok := cache.Get("two"); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if cache.Len() != 2 {
		t.Fatalf("cache length = %d", cache.Len())
	}
	cache.Set("short", "value", 5*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, ok := cache.Get("short"); ok {
		t.Fatal("expired entry was returned")
	}
}
