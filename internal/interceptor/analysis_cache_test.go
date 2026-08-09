package interceptor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

func TestIdempotencyCacheRejectsWhenAllCapacityIsPending(t *testing.T) {
	cache := newIdempotencyCache(2)
	first, _, _, err := cache.Reserve(context.Background(), "call_1", "identity_1")
	if err != nil || first == nil {
		t.Fatalf("first reservation=%#v err=%v", first, err)
	}
	second, _, _, err := cache.Reserve(context.Background(), "call_2", "identity_2")
	if err != nil || second == nil {
		t.Fatalf("second reservation=%#v err=%v", second, err)
	}
	if _, _, _, err := cache.Reserve(context.Background(), "call_3", "identity_3"); !errors.Is(err, errIdempotencyCacheFull) {
		t.Fatalf("full cache err=%v", err)
	}
	if got := cache.Size(); got != 2 {
		t.Fatalf("pending cache size=%d, want 2", got)
	}

	cache.Complete(first, "analysis", nil, time.Minute)
	third, _, hit, err := cache.Reserve(context.Background(), "call_3", "identity_3")
	if err != nil || third == nil || hit {
		t.Fatalf("third reservation=%#v hit=%v err=%v", third, hit, err)
	}
	if got := cache.Size(); got != 2 {
		t.Fatalf("cache size after completed eviction=%d, want 2", got)
	}
	cache.Complete(second, "", errIdempotencyReservationAborted, 0)
	cache.Complete(third, "", errIdempotencyReservationAborted, 0)
}

func TestIdempotencyCacheConcurrentPendingReservationsStayBounded(t *testing.T) {
	const capacity = 4
	const callers = 32
	cache := newIdempotencyCache(capacity)
	start := make(chan struct{})
	type result struct {
		reservation *idempotencyReservation
		err         error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			reservation, _, _, err := cache.Reserve(context.Background(), fmt.Sprintf("call_%d", index), fmt.Sprintf("identity_%d", index))
			results <- result{reservation: reservation, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	reservations := make([]*idempotencyReservation, 0, capacity)
	full := 0
	for got := range results {
		switch {
		case got.err == nil && got.reservation != nil:
			reservations = append(reservations, got.reservation)
		case errors.Is(got.err, errIdempotencyCacheFull):
			full++
		default:
			t.Fatalf("unexpected reservation result: %#v", got)
		}
	}
	if len(reservations) != capacity || full != callers-capacity || cache.Size() != capacity {
		t.Fatalf("reservations=%d full=%d size=%d", len(reservations), full, cache.Size())
	}
	for _, reservation := range reservations {
		cache.Complete(reservation, "", errIdempotencyReservationAborted, 0)
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
