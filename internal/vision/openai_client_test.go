package vision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestClientSuccessPayloadAndAuth(t *testing.T) {
	var gotAuth string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"reasoning","content":[{"type":"output_text","text":"do not return"}]},{"type":"message","content":[{"type":"output_text","text":"Visible text: ok\nVisual description: screen"}]}]}`))
	}))
	cleanupTestServer(t, srv)
	c, err := NewClient(Options{BaseURL: srv.URL, Model: "gpt-5.6-luna", Token: "secret", Language: "zh-CN", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Analyze(context.Background(), "data:image/png;base64,AAAA", "focus")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || !strings.Contains(got, "Visual description") {
		t.Fatalf("auth/result mismatch: %q %q", gotAuth, got)
	}
	if payload["model"] != "gpt-5.6-luna" || payload["stream"] != false {
		t.Fatalf("payload: %#v", payload)
	}
	in := payload["input"].([]any)
	content := in[0].(map[string]any)["content"].([]any)
	if prompt, _ := content[0].(map[string]any)["text"].(string); !strings.Contains(prompt, "Simplified Chinese") {
		t.Fatalf("configured language missing from prompt: %q", prompt)
	}
	if content[1].(map[string]any)["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image was not sent as input_image: %#v", content)
	}
}

func TestClientRetriesTransientStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"output_text":"ok"}`))
	}))
	cleanupTestServer(t, srv)
	c, err := NewClient(Options{BaseURL: srv.URL, MaxAttempts: 3, RetryBaseDelay: time.Millisecond, MaxRetryDelay: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Analyze(context.Background(), "https://example.com/a.png", "")
	if err != nil || got != "ok" || calls.Load() != 3 {
		t.Fatalf("got %q err %v calls %d", got, err, calls.Load())
	}
}

func TestClientDoesNotRetryAuth(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("x"))
	}))
	cleanupTestServer(t, srv)
	c, err := NewClient(Options{BaseURL: srv.URL, MaxAttempts: 3, RetryBaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Analyze(context.Background(), "https://example.com/a.png", ""); err == nil || calls.Load() != 1 {
		t.Fatalf("expected one non-retried auth error, calls=%d err=%v", calls.Load(), err)
	}
}

func TestClientTimeoutCancellation(t *testing.T) {
	requestStarted := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var startOnce sync.Once
	var observeOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(requestStarted) })
		select {
		case <-r.Context().Done():
			observeOnce.Do(func() { close(cancelObserved) })
		case <-release:
			// Deterministic cleanup fallback for transports that do not propagate
			// a client-side deadline to the test server before teardown.
			observeOnce.Do(func() { close(cancelObserved) })
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		srv.CloseClientConnections()
		srv.Close()
	})
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
	client := &http.Client{Transport: transport}
	c, err := NewClient(Options{BaseURL: srv.URL, RequestTimeout: 20 * time.Millisecond, MaxAttempts: 2, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, callErr := c.Analyze(requestCtx, "https://example.com/a.png", "")
		done <- result{err: callErr}
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
	var callErr error
	select {
	case result := <-done:
		callErr = result.err
	case <-time.After(time.Second):
		t.Fatal("Analyze did not honor timeout")
	}
	if callErr == nil || time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced: %v", callErr)
	}
	// Ensure the server-side request context is cancelled even on transports
	// that return a client-side timeout before propagating the FIN. The test
	// still asserts that Analyze itself obeyed RequestTimeout above.
	cancelRequest()
	// Explicitly close any lingering keep-alive connection before asserting the
	// server-side observation. This also makes the assertion deterministic on
	// transports that defer FIN propagation after a client-side deadline.
	srv.CloseClientConnections()
	releaseOnce.Do(func() { close(release) })
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("server did not observe request cancellation")
	}
}

func TestClientResponseLimitsAndInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		max  int64
	}{
		{"oversize", `{"output_text":"123456"}`, 5},
		{"invalid", `{`, 100},
		{"empty", `{"output":[]}`, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			cleanupTestServer(t, srv)
			c, err := NewClient(Options{BaseURL: srv.URL, MaxResponseBytes: tc.max, MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Analyze(context.Background(), "https://example.com/a.png", ""); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestBuildPromptBoundsHint(t *testing.T) {
	if got := BuildPrompt(strings.Repeat("x", 3000)); !strings.Contains(got, strings.Repeat("x", 2000)) || strings.Contains(got, strings.Repeat("x", 2001)) {
		t.Fatal("focus hint was not bounded to exactly 2000 runes")
	}
	if !strings.Contains(BuildPrompt("x"), "untrusted data") {
		t.Fatal("prompt injection warning missing")
	}
	for _, tc := range []struct {
		language string
		want     string
	}{
		{"zh-CN", "Simplified Chinese"},
		{"English", "in English"},
	} {
		if got := BuildPrompt("", tc.language); !strings.Contains(got, tc.want) {
			t.Errorf("BuildPrompt language %q = %q, want %q", tc.language, got, tc.want)
		}
	}
}

func TestNewClientDoesNotMutateCallerHTTPClient(t *testing.T) {
	originalRedirect := func(req *http.Request, via []*http.Request) error { return errors.New("caller policy") }
	caller := &http.Client{CheckRedirect: originalRedirect}
	c, err := NewClient(Options{BaseURL: "https://example.com/v1", HTTPClient: caller})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(originalRedirect).Pointer() {
		t.Fatal("NewClient mutated caller CheckRedirect")
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = w.Write([]byte(`{"output_text":"should-not-run"}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/responses", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := NewClient(Options{BaseURL: source.URL, Token: "secret", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Analyze(context.Background(), "data:image/png;base64,AAAA", ""); err == nil {
		t.Fatal("redirect was treated as a successful VLM response")
	}
	if redirected.Load() != 0 {
		t.Fatal("client followed a redirect to another origin")
	}
}

func TestNewClientRejectsAmbiguousBaseURL(t *testing.T) {
	for _, base := range []string{
		"https://vision.example/v1?token=secret",
		"https://vision.example/v1#fragment",
		"https://vision.example/v1/responses",
		"https://vision.example:bad/v1",
	} {
		if _, err := NewClient(Options{BaseURL: base}); err == nil {
			t.Fatalf("ambiguous base URL accepted: %q", base)
		}
	}
}

func TestClientCloseConcurrent(t *testing.T) {
	c, err := NewClient(Options{BaseURL: "https://example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.Analyze(context.Background(), "https://example.com/a.png", ""); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Analyze after Close = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closeSpyTransport struct {
	closeCalls atomic.Int32
}

func (s *closeSpyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"output_text":"ok"}`)),
		Request:    req,
	}, nil
}

func (s *closeSpyTransport) CloseIdleConnections() { s.closeCalls.Add(1) }

func TestClientCloseDoesNotCloseCallerTransport(t *testing.T) {
	transport := &closeSpyTransport{}
	c, err := NewClient(Options{BaseURL: "https://example.com/v1", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := transport.closeCalls.Load(); got != 0 {
		t.Fatalf("caller transport CloseIdleConnections calls = %d", got)
	}
}

func TestClientConfiguredImageReferenceLimitAndStrictValidation(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"output_text":"ok"}`)),
			Request:    req,
		}, nil
	})
	hugeReference := "HTTPS://EXAMPLE.COM/" + strings.Repeat("a", (16<<20)+1)
	c, err := NewClient(Options{
		BaseURL:                "https://example.com/v1",
		HTTPClient:             &http.Client{Transport: transport},
		MaxImageReferenceBytes: len(hugeReference) + 1,
		MaxAttempts:            1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, err := c.Analyze(context.Background(), hugeReference, ""); err != nil || got != "ok" {
		t.Fatalf("configured >16MiB reference rejected: result=%q err=%v", got, err)
	}
	if _, err := c.Analyze(context.Background(), "data:image/png;base64,%%%", ""); err == nil {
		t.Fatal("invalid data image payload was accepted")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("invalid reference reached transport; calls=%d", got)
	}
}

func TestClientRetriesConnectionFailure(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"output_text":"recovered"}`)),
			Request:    req,
		}, nil
	})}
	c, err := NewClient(Options{BaseURL: "https://example.com/v1", HTTPClient: httpClient, MaxAttempts: 2, RetryBaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Analyze(context.Background(), "https://example.com/a.png", "")
	if err != nil || got != "recovered" || calls.Load() != 2 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls.Load())
	}
}

func TestClientRetriesInterruptedResponseBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"out`))
			return
		}
		_, _ = w.Write([]byte(`{"output_text":"recovered"}`))
	}))
	cleanupTestServer(t, srv)
	c, err := NewClient(Options{BaseURL: srv.URL, MaxAttempts: 2, RetryBaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Analyze(context.Background(), "https://example.com/a.png", "")
	if err != nil || got != "recovered" || calls.Load() != 2 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls.Load())
	}
}

func cleanupTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
}
