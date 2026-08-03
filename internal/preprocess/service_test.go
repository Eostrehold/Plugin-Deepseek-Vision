package preprocess

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/cache"
)

type fakeAnalyzer struct {
	mu        sync.Mutex
	calls     int
	delay     time.Duration
	err       error
	results   map[string]string
	active    int
	maxActive int
}

func (f *fakeAnalyzer) Analyze(ctx context.Context, ref, focus string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if f.delay > 0 {
		t := time.NewTimer(f.delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if f.results != nil {
		return f.results[ref], nil
	}
	return "result:" + ref, nil
}

func (f *fakeAnalyzer) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestAnalyzeAllPreservesOrderAndBoundsConcurrency(t *testing.T) {
	f := &fakeAnalyzer{delay: 20 * time.Millisecond}
	s, err := NewService(Options{Analyzer: f, MaxConcurrency: 2, MaxImages: 4, CacheCapacity: 0})
	if err != nil {
		t.Fatal(err)
	}
	cleanupService(t, s)
	got, err := s.AnalyzeAll(context.Background(), []Image{{"https://example.com/1.png"}, {"https://example.com/2.png"}, {"https://example.com/3.png"}}, "")
	if err != nil || len(got) != 3 || got[0] != "result:https://example.com/1.png" || f.maxActive > 2 {
		t.Fatalf("got=%v err=%v maxActive=%d", got, err, f.maxActive)
	}
}

func TestDuplicateCallsCoalesceAndCache(t *testing.T) {
	f := &fakeAnalyzer{delay: 30 * time.Millisecond}
	s, _ := NewService(Options{Analyzer: f, MaxConcurrency: 2, CacheCapacity: 8, CacheTTL: time.Minute, Model: "m", ConfigGeneration: "g"})
	cleanupService(t, s)
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AnalyzeOne(context.Background(), Image{"https://example.com/a.png"}, "hint"); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 || f.Calls() != 1 {
		t.Fatalf("coalescing failed failures=%d calls=%d", failures.Load(), f.Calls())
	}
	if _, err := s.AnalyzeOne(context.Background(), Image{"https://example.com/a.png"}, "hint"); err != nil || f.Calls() != 1 {
		t.Fatalf("cache miss after success calls=%d err=%v", f.Calls(), err)
	}
}

func TestCacheTTLAndGeneration(t *testing.T) {
	f := &fakeAnalyzer{}
	s, _ := NewService(Options{Analyzer: f, CacheCapacity: 8, CacheTTL: 10 * time.Millisecond, Model: "m", ConfigGeneration: "1"})
	cleanupService(t, s)
	image := Image{"https://example.com/a.png"}
	_, _ = s.AnalyzeOne(context.Background(), image, "")
	_, _ = s.AnalyzeOne(context.Background(), image, "")
	if f.Calls() != 1 {
		t.Fatalf("expected cached call, got %d", f.Calls())
	}
	time.Sleep(20 * time.Millisecond)
	_, _ = s.AnalyzeOne(context.Background(), image, "")
	if f.Calls() != 2 {
		t.Fatalf("expected TTL expiry, got %d", f.Calls())
	}
	s.ClearCache()
	if s.cache.Len() != 0 {
		t.Fatal("cache was not cleared")
	}
}

func TestAnalyzeAllFailureHasNoPartialResults(t *testing.T) {
	f := &fakeAnalyzer{err: errors.New("boom")}
	s, _ := NewService(Options{Analyzer: f, MaxImages: 2})
	cleanupService(t, s)
	got, err := s.AnalyzeAll(context.Background(), []Image{{"https://example.com/a.png"}}, "")
	if err == nil || got != nil {
		t.Fatalf("expected no partial result got=%v err=%v", got, err)
	}
}

func TestServiceLimitsAndClose(t *testing.T) {
	f := &fakeAnalyzer{}
	s, _ := NewService(Options{Analyzer: f, MaxImages: 1, MaxImageReferenceBytes: 10})
	cleanupService(t, s)
	if _, err := s.AnalyzeAll(context.Background(), []Image{{"https://example.com/a.png"}, {"https://example.com/b.png"}}, ""); err == nil {
		t.Fatal("expected image count limit")
	}
	if _, err := s.AnalyzeOne(context.Background(), Image{"file:///tmp/a"}, ""); err == nil {
		t.Fatal("expected unsupported reference error")
	}
	_ = s.Close()
	if _, err := s.AnalyzeOne(context.Background(), Image{"https://a.co/a"}, ""); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("expected closed error, got %v", err)
	}
}

func TestCacheKeySeparatesParts(t *testing.T) {
	if cache.Key("ab", "c") == cache.Key("a", "bc") {
		t.Fatal("ambiguous cache key")
	}
}

type controlledAnalyzer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (a *controlledAnalyzer) Analyze(ctx context.Context, ref, focus string) (string, error) {
	a.calls.Add(1)
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		return "shared result", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestOwnerCancellationDoesNotCancelIndependentWaiter(t *testing.T) {
	a := &controlledAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	s, err := NewService(Options{Analyzer: a, CacheCapacity: 8, CacheTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cleanupService(t, s)
	image := Image{"https://example.com/shared.png"}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() {
		_, callErr := s.AnalyzeOne(ownerCtx, image, "focus")
		ownerDone <- callErr
	}()
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}

	waiterDone := make(chan struct {
		result string
		err    error
	}, 1)
	go func() {
		result, callErr := s.AnalyzeOne(context.Background(), image, "focus")
		waiterDone <- struct {
			result string
			err    error
		}{result, callErr}
	}()
	cancelOwner()
	select {
	case callErr := <-ownerDone:
		if !errors.Is(callErr, context.Canceled) {
			t.Fatalf("owner error = %v", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not observe cancellation")
	}
	close(a.release)
	select {
	case got := <-waiterDone:
		if got.err != nil || got.result != "shared result" {
			t.Fatalf("waiter result=%q err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent waiter did not receive shared result")
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("analyzer calls = %d, want 1", got)
	}
}

type stubbornAnalyzer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *stubbornAnalyzer) Analyze(context.Context, string, string) (string, error) {
	a.once.Do(func() { close(a.started) })
	<-a.release
	return "late success", nil
}

func TestCloseIsInflightBarrierAndSuppressesLateSuccess(t *testing.T) {
	a := &stubbornAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	resultCache := cache.NewLRU(8, time.Minute)
	s, err := NewService(Options{Analyzer: a, Cache: resultCache})
	if err != nil {
		t.Fatal(err)
	}
	callDone := make(chan error, 1)
	go func() {
		_, callErr := s.AnalyzeOne(context.Background(), Image{"https://example.com/late.png"}, "")
		callDone <- callErr
	}()
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight analyzer completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(a.release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not complete after worker release")
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("late call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting caller did not complete")
	}
	if resultCache.Len() != 0 {
		t.Fatal("late success was published to cache after Close")
	}
}

type panicAnalyzer struct{}

func (panicAnalyzer) Analyze(context.Context, string, string) (string, error) {
	panic("secret-image-reference")
}

func TestAnalyzerPanicIsRecoveredAndSanitized(t *testing.T) {
	s, err := NewService(Options{Analyzer: panicAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupService(t, s)
	_, err = s.AnalyzeOne(context.Background(), Image{"https://example.com/a.png"}, "")
	if !errors.Is(err, ErrAnalyzerPanic) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("panic error not sanitized: %v", err)
	}
}

func TestLanguageSeparatesCacheKeys(t *testing.T) {
	f := &fakeAnalyzer{}
	shared := cache.NewLRU(8, time.Minute)
	zh, _ := NewService(Options{Analyzer: f, Cache: shared, Model: "m", Language: "zh-CN"})
	en, _ := NewService(Options{Analyzer: f, Cache: shared, Model: "m", Language: "English"})
	cleanupService(t, zh)
	cleanupService(t, en)
	image := Image{"https://example.com/language.png"}
	if _, err := zh.AnalyzeOne(context.Background(), image, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := en.AnalyzeOne(context.Background(), image, ""); err != nil {
		t.Fatal(err)
	}
	if f.Calls() != 2 {
		t.Fatalf("different languages shared a cache entry; calls=%d", f.Calls())
	}
}

type trackingLimiter struct {
	acquired    chan struct{}
	released    chan struct{}
	acquireOnce sync.Once
	releaseOnce sync.Once
}

func (l *trackingLimiter) Acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	l.acquireOnce.Do(func() { close(l.acquired) })
	return nil
}

func (l *trackingLimiter) Release() {
	l.releaseOnce.Do(func() { close(l.released) })
}

func TestCanceledWaiterDoesNotReleaseSharedLimiter(t *testing.T) {
	a := &controlledAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	limiter := &trackingLimiter{acquired: make(chan struct{}), released: make(chan struct{})}
	s, err := NewService(Options{Analyzer: a, Limiter: limiter, MaxConcurrency: 99})
	if err != nil {
		t.Fatal(err)
	}
	cleanupService(t, s)
	image := Image{"https://example.com/limited.png"}
	producerDone := make(chan error, 1)
	go func() {
		_, callErr := s.AnalyzeOne(context.Background(), image, "")
		producerDone <- callErr
	}()
	select {
	case <-limiter.acquired:
	case <-time.After(time.Second):
		t.Fatal("shared limiter was not acquired")
	}
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, callErr := s.AnalyzeOne(waiterCtx, image, "")
		waiterDone <- callErr
	}()
	cancelWaiter()
	select {
	case callErr := <-waiterDone:
		if !errors.Is(callErr, context.Canceled) {
			t.Fatalf("waiter error = %v", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	select {
	case <-limiter.released:
		t.Fatal("canceled waiter released limiter while producer was active")
	case <-time.After(20 * time.Millisecond):
	}

	close(a.release)
	select {
	case callErr := <-producerDone:
		if callErr != nil {
			t.Fatalf("producer error = %v", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not finish")
	}
	select {
	case <-limiter.released:
	case <-time.After(time.Second):
		t.Fatal("limiter was not released after analyzer completed")
	}
}

func cleanupService(t *testing.T, s *Service) {
	t.Helper()
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			_ = s.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Service.Close timed out during cleanup")
		}
	})
}
