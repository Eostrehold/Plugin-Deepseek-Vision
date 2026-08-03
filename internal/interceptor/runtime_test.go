package interceptor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/preprocess"
)

type testAnalyzer struct {
	mu        sync.Mutex
	focus     []string
	refs      []string
	err       error
	started   chan struct{}
	continueC chan struct{}
}

type controlledAnalyzer struct {
	mu        sync.Mutex
	active    int
	maxActive int
	entered   chan string
	release   map[string]chan struct{}
	ignoreCtx bool
}

func (a *controlledAnalyzer) Analyze(ctx context.Context, ref, _ string) (string, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.maxActive {
		a.maxActive = a.active
	}
	release := a.release[ref]
	a.mu.Unlock()
	a.entered <- ref
	defer func() {
		a.mu.Lock()
		a.active--
		a.mu.Unlock()
	}()
	if a.ignoreCtx {
		<-release
		return "Visible text: done\nVisual description: done", nil
	}
	select {
	case <-release:
		return "Visible text: done\nVisual description: done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *testAnalyzer) Analyze(ctx context.Context, ref, focus string) (string, error) {
	a.mu.Lock()
	a.refs = append(a.refs, ref)
	a.focus = append(a.focus, focus)
	started := a.started
	continueC := a.continueC
	err := a.err
	a.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if continueC != nil {
		select {
		case <-continueC:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return "Visible text: " + ref + "\nVisual description: focus=" + focus, nil
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.ParseYAML([]byte(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_base_url: http://127.0.0.1:1/v1
vision_model: gpt-5.6-luna
vision_api_key_env: TEST_VISION_KEY
language: zh
request_timeout_seconds: 2
per_call_timeout_seconds: 1
retry_max_attempts: 1
max_concurrency: 4
max_images_per_request: 4
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
cache_size: 0
cache_ttl_seconds: 60
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestRuntime(t *testing.T, analyzer *testAnalyzer) *Runtime {
	t.Helper()
	r := NewRuntime(func(cfg *config.Config, _ uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
		return preprocess.NewService(preprocess.Options{
			Analyzer: analyzer, MaxConcurrency: cfg.MaxConcurrency,
			MaxImages:              cfg.MaxImagesPerRequest,
			MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
			MaxResultChars:         cfg.MaxResultChars,
			Limiter:                limiter,
		})
	})
	r.Reconfigure(testConfig(t))
	return r
}

func configWith(t *testing.T, concurrency int, language string) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.MaxConcurrency = concurrency
	cfg.Language = language
	return cfg
}

func imageBody(refs ...string) string {
	var blocks []string
	for _, ref := range refs {
		blocks = append(blocks, `{"type":"input_image","image_url":"`+ref+`"}`)
	}
	return `{"input":[{"role":"user","content":[` + strings.Join(blocks, ",") + `]}]}`
}

func makeRequest(model, source, path, body string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		SourceFormat: source, ToFormat: "openai-response", Model: model,
		RequestedModel: "deepseek-pro", Body: []byte(body),
		Headers:  http.Header{"X-Test": []string{"keep"}},
		Metadata: map[string]any{"request_path": path},
	}
}

func TestHandleRewritesContentAndFunctionOutputWithIndividualFocus(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"first focus"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_text","text":"second focus"},{"type":"input_image","image_url":"https://example.com/two.png"}]},{"type":"function_call_output","output":[{"type":"input_image","image_url":"https://example.com/three.png"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-pro", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || string(resp.Body) == body {
		t.Fatalf("response err=%v terminate=%v body=%s", err, resp.Terminate, resp.Body)
	}
	if strings.Contains(string(resp.Body), "input_image") || strings.Contains(string(resp.Body), "data:image") {
		t.Fatalf("rewritten body retained image reference: %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), "[Image 1 — Visual analysis]") || !strings.Contains(string(resp.Body), "[Image 3 — Visual analysis]") {
		t.Fatalf("missing replacement blocks: %s", resp.Body)
	}
	analyzer.mu.Lock()
	gotFocus := append([]string(nil), analyzer.focus...)
	analyzer.mu.Unlock()
	if len(gotFocus) != 3 {
		t.Fatalf("analyzer calls=%d", len(gotFocus))
	}
	for _, focus := range gotFocus {
		if focus == "" {
			t.Fatal("missing per-image focus hint")
		}
	}
}

func TestHandleRewritesVisibleHistoryAndCurrentImagesForFinalTarget(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()

	historyRef := "https://example.com/codex-luna-history.png"
	currentRef := "https://example.com/current-flash.png"
	body := `{"model":"deepseek-v4-flash","previous_response_id":"resp_codex_luna_history","input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"Codex Luna history"},{"type":"input_image","image_url":"` + historyRef + `"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"Describe the current screenshot."},{"type":"input_image","image_url":"` + currentRef + `"}]}]}`

	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate {
		t.Fatalf("history/current request failed: err=%v response=%#v", err, resp)
	}
	rewritten := string(resp.Body)
	for _, ref := range []string{historyRef, currentRef} {
		if strings.Contains(rewritten, ref) {
			t.Fatalf("rewritten body retained original image reference %q: %s", ref, rewritten)
		}
	}
	if strings.Contains(rewritten, "input_image") ||
		!strings.Contains(rewritten, "[Image 1 — Visual analysis]") ||
		!strings.Contains(rewritten, "[Image 2 — Visual analysis]") ||
		!strings.Contains(rewritten, "Codex Luna history") ||
		!strings.Contains(rewritten, "resp_codex_luna_history") {
		t.Fatalf("history/current images were not both replaced: %s", rewritten)
	}

	analyzer.mu.Lock()
	refs := append([]string(nil), analyzer.refs...)
	analyzer.mu.Unlock()
	if len(refs) != 2 {
		t.Fatalf("analyzer calls=%d refs=%v, want historical and current images", len(refs), refs)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		seen[ref] = true
	}
	if !seen[historyRef] || !seen[currentRef] {
		t.Fatalf("analyzer refs=%v, want %q and %q", refs, historyRef, currentRef)
	}

	// A final model outside target_models must leave the complete visible
	// history/current request untouched and must not invoke the VLM.
	before := []byte(body)
	resp, err = r.Handle(makeRequest("deepseek-other", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || string(resp.Body) != string(before) {
		t.Fatalf("non-target history/current request was not passthrough: err=%v response=%#v", err, resp)
	}
	analyzer.mu.Lock()
	callCount := len(analyzer.refs)
	analyzer.mu.Unlock()
	if callCount != 2 {
		t.Fatalf("non-target request invoked analyzer: refs=%v", refs)
	}
}

func TestHandleGateUsesFinalModelAndExactPath(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	cases := []struct {
		name    string
		req     pluginapi.RequestInterceptRequest
		rewrite bool
	}{
		{"alias requested_but_target_final", makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body), true},
		{"non_target_final", makeRequest("deepseek-other", "openai-response", "/v1/responses", body), false},
		{"compact", makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses/compact", body), false},
		{"wrong_source", makeRequest("deepseek-v4-flash", "openai-request", "/v1/responses", body), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.req.Body
			resp, err := r.Handle(tc.req)
			if err != nil || resp.Terminate || (tc.rewrite == (string(resp.Body) == string(before))) {
				t.Fatalf("err=%v terminate=%v body=%s", err, resp.Terminate, resp.Body)
			}
		})
	}
	// RequestedModel is intentionally ignored when the final Model is not a
	// target; this is the alias regression that the gate must prevent.
	req := makeRequest("provider-final", "openai-response", "/v1/responses", body)
	req.RequestedModel = "deepseek-v4-pro"
	resp, _ := r.Handle(req)
	if resp.Terminate || string(resp.Body) != body {
		t.Fatalf("requested model incorrectly opened gate: %#v", resp)
	}
}

func TestHandleMapsPlannerErrorsAndVLMFailuresWithoutBody(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{})
		defer r.Shutdown()
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", `{`))
		if !resp.Terminate || resp.StatusCode != http.StatusBadRequest || len(resp.Body) != 0 {
			t.Fatalf("response=%#v", resp)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{})
		defer r.Shutdown()
		body := `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		if !resp.Terminate || resp.StatusCode != http.StatusUnprocessableEntity || len(resp.Body) != 0 {
			t.Fatalf("response=%#v", resp)
		}
	})
	t.Run("vlm", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{err: errors.New("secret endpoint text")})
		defer r.Shutdown()
		body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		if !resp.Terminate || resp.StatusCode != http.StatusBadGateway || len(resp.Body) != 0 || strings.Contains(string(resp.ResponseBody), "secret") {
			t.Fatalf("response=%#v body=%s", resp, resp.ResponseBody)
		}
	})
}

func TestRuntimeReconfigureAndShutdownAreSafe(t *testing.T) {
	analyzer := &testAnalyzer{started: make(chan struct{}, 1), continueC: make(chan struct{})}
	r := newTestRuntime(t, analyzer)
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	done := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		done <- resp
	}()
	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	r.Shutdown()
	close(analyzer.continueC)
	resp := <-done
	if resp.Terminate {
		t.Fatalf("in-flight request was terminated during shutdown: %#v", resp)
	}
	resp, _ = r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if !resp.Terminate || resp.StatusCode != http.StatusBadGateway || len(resp.Body) != 0 {
		t.Fatalf("shutdown did not fail closed for new image requests: %#v", resp)
	}
	r.Shutdown()
}

func TestRuntimeGlobalLimitAcrossRetiredAndCurrentGenerations(t *testing.T) {
	refs := []string{"https://example.com/old-1.png", "https://example.com/old-2.png", "https://example.com/new.png"}
	analyzer := &controlledAnalyzer{entered: make(chan string, 4), release: make(map[string]chan struct{}, len(refs))}
	for _, ref := range refs {
		analyzer.release[ref] = make(chan struct{})
	}
	r := NewRuntime(func(cfg *config.Config, _ uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
		return preprocess.NewService(preprocess.Options{
			Analyzer: analyzer, MaxConcurrency: cfg.MaxConcurrency,
			MaxImages: cfg.MaxImagesPerRequest, MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
			MaxResultChars: cfg.MaxResultChars, Language: cfg.Language, Limiter: limiter,
		})
	})
	r.Reconfigure(configWith(t, 2, "zh-CN"))
	defer r.Shutdown()

	oldDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(refs[0], refs[1])))
		oldDone <- resp
	}()
	entered := map[string]bool{}
	for len(entered) < 2 {
		select {
		case ref := <-analyzer.entered:
			entered[ref] = true
		case <-time.After(time.Second):
			t.Fatal("old generation did not fill the initial limit")
		}
	}

	// Lowering to one grandfathers the two active calls, but the current
	// generation cannot enter until both have drained below the new limit.
	r.Reconfigure(configWith(t, 1, "en"))
	newDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(refs[2])))
		newDone <- resp
	}()
	assertNoEntry := func(label string) {
		t.Helper()
		select {
		case ref := <-analyzer.entered:
			t.Fatalf("%s: unexpectedly admitted %s", label, ref)
		case <-time.After(30 * time.Millisecond):
		}
	}
	assertNoEntry("while two old calls are active")
	close(analyzer.release[refs[0]])
	assertNoEntry("while active equals lowered limit")
	close(analyzer.release[refs[1]])
	select {
	case ref := <-analyzer.entered:
		if ref != refs[2] {
			t.Fatalf("entered=%s", ref)
		}
	case <-time.After(time.Second):
		t.Fatal("current generation was not admitted after old calls drained")
	}
	close(analyzer.release[refs[2]])
	if resp := <-oldDone; resp.Terminate {
		t.Fatalf("old response terminated: %#v", resp)
	}
	if resp := <-newDone; resp.Terminate {
		t.Fatalf("new response terminated: %#v", resp)
	}
	analyzer.mu.Lock()
	maxActive := analyzer.maxActive
	analyzer.mu.Unlock()
	if maxActive != 2 {
		t.Fatalf("combined max active=%d, want initial policy 2", maxActive)
	}
}

func TestRuntimeShutdownWakesCrossRequestWaiter(t *testing.T) {
	oldRef := "https://example.com/active.png"
	waitRef := "https://example.com/waiting.png"
	analyzer := &controlledAnalyzer{entered: make(chan string, 2), release: map[string]chan struct{}{
		oldRef: make(chan struct{}), waitRef: make(chan struct{}),
	}}
	r := NewRuntime(func(cfg *config.Config, _ uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
		return preprocess.NewService(preprocess.Options{Analyzer: analyzer, MaxConcurrency: cfg.MaxConcurrency, MaxImages: 2, MaxImageReferenceBytes: cfg.MaxImageReferenceBytes, MaxResultChars: cfg.MaxResultChars, Limiter: limiter})
	})
	r.Reconfigure(configWith(t, 1, "zh"))
	activeDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(oldRef)))
		activeDone <- resp
	}()
	select {
	case ref := <-analyzer.entered:
		if ref != oldRef {
			t.Fatalf("entered=%s", ref)
		}
	case <-time.After(time.Second):
		t.Fatal("active call did not enter")
	}
	waitDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(waitRef)))
		waitDone <- resp
	}()
	select {
	case ref := <-analyzer.entered:
		t.Fatalf("waiter unexpectedly entered: %s", ref)
	case <-time.After(30 * time.Millisecond):
	}
	r.Shutdown()
	select {
	case resp := <-waitDone:
		if !resp.Terminate || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("shutdown waiter response=%#v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wake limiter waiter")
	}
	close(analyzer.release[oldRef])
	if resp := <-activeDone; resp.Terminate {
		t.Fatalf("grandfathered active response terminated: %#v", resp)
	}
}

func TestProducerKeepsGlobalSlotAfterCallerTimeoutAcrossReconfigure(t *testing.T) {
	oldRef := "https://example.com/orphaned-producer.png"
	newRef := "https://example.com/new-generation.png"
	analyzer := &controlledAnalyzer{
		entered: make(chan string, 2), ignoreCtx: true,
		release: map[string]chan struct{}{oldRef: make(chan struct{}), newRef: make(chan struct{})},
	}
	r := NewRuntime(func(cfg *config.Config, _ uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
		return preprocess.NewService(preprocess.Options{
			Analyzer: analyzer, MaxConcurrency: cfg.MaxConcurrency, Limiter: limiter,
			MaxImages: 2, MaxImageReferenceBytes: cfg.MaxImageReferenceBytes, MaxResultChars: cfg.MaxResultChars,
		})
	})
	oldCfg := configWith(t, 1, "zh")
	oldCfg.RequestTimeout = 25 * time.Millisecond
	r.Reconfigure(oldCfg)

	oldCallerDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(oldRef)))
		oldCallerDone <- resp
	}()
	select {
	case ref := <-analyzer.entered:
		if ref != oldRef {
			t.Fatalf("entered=%s", ref)
		}
	case <-time.After(time.Second):
		t.Fatal("old producer did not enter")
	}
	select {
	case resp := <-oldCallerDone:
		if !resp.Terminate || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("timed-out caller response=%#v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("request caller did not time out")
	}

	newCfg := configWith(t, 1, "en")
	reconfigureDone := make(chan struct{})
	go func() {
		r.Reconfigure(newCfg)
		close(reconfigureDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		generation := uint64(0)
		if r.current != nil {
			generation = r.current.generation
		}
		r.mu.Unlock()
		if generation >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new generation was not published")
		}
		time.Sleep(time.Millisecond)
	}
	newDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(newRef)))
		newDone <- resp
	}()
	select {
	case ref := <-analyzer.entered:
		t.Fatalf("new producer entered before orphaned producer ended: %s", ref)
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case <-reconfigureDone:
		t.Fatal("retired service closed before its producer ended")
	default:
	}

	close(analyzer.release[oldRef])
	select {
	case <-reconfigureDone:
	case <-time.After(time.Second):
		t.Fatal("reconfigure did not finish after old producer ended")
	}
	select {
	case ref := <-analyzer.entered:
		if ref != newRef {
			t.Fatalf("entered=%s", ref)
		}
	case <-time.After(time.Second):
		t.Fatal("new producer did not enter after old producer ended")
	}
	close(analyzer.release[newRef])
	if resp := <-newDone; resp.Terminate {
		t.Fatalf("new response terminated: %#v", resp)
	}
	analyzer.mu.Lock()
	maxActive := analyzer.maxActive
	analyzer.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("HTTP/analyzer max active=%d, want 1", maxActive)
	}
	r.Shutdown()
}

func TestDuplicateWaitersShareProducerAndOneGlobalSlot(t *testing.T) {
	dupRef := "https://example.com/duplicate.png"
	otherRef := "https://example.com/other.png"
	analyzer := &controlledAnalyzer{entered: make(chan string, 3), release: map[string]chan struct{}{
		dupRef: make(chan struct{}), otherRef: make(chan struct{}),
	}}
	r := NewRuntime(func(cfg *config.Config, _ uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
		return preprocess.NewService(preprocess.Options{
			Analyzer: analyzer, MaxConcurrency: cfg.MaxConcurrency, Limiter: limiter,
			MaxImages: 2, MaxImageReferenceBytes: cfg.MaxImageReferenceBytes, MaxResultChars: cfg.MaxResultChars,
		})
	})
	r.Reconfigure(configWith(t, 1, "zh"))
	defer r.Shutdown()
	firstDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(dupRef)))
		firstDone <- resp
	}()
	select {
	case <-analyzer.entered:
	case <-time.After(time.Second):
		t.Fatal("duplicate producer did not enter")
	}
	secondDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(dupRef)))
		secondDone <- resp
	}()
	otherDone := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", imageBody(otherRef)))
		otherDone <- resp
	}()
	select {
	case ref := <-analyzer.entered:
		t.Fatalf("extra producer entered while duplicate occupied one slot: %s", ref)
	case <-time.After(40 * time.Millisecond):
	}
	close(analyzer.release[dupRef])
	for _, done := range []chan pluginapi.RequestInterceptResponse{firstDone, secondDone} {
		if resp := <-done; resp.Terminate {
			t.Fatalf("duplicate response terminated: %#v", resp)
		}
	}
	select {
	case ref := <-analyzer.entered:
		if ref != otherRef {
			t.Fatalf("entered=%s", ref)
		}
	case <-time.After(time.Second):
		t.Fatal("other producer did not enter after duplicate producer finished")
	}
	close(analyzer.release[otherRef])
	if resp := <-otherDone; resp.Terminate {
		t.Fatalf("other response terminated: %#v", resp)
	}
	analyzer.mu.Lock()
	maxActive := analyzer.maxActive
	analyzer.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max active=%d, want 1", maxActive)
	}
}

func TestDynamicLimiterHonorsContextCancellation(t *testing.T) {
	limiter := newDynamicLimiter()
	limiter.configure(1)
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- limiter.Acquire(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not wake waiter")
	}
	limiter.Release()
	limiter.shutdown()
}

func TestFingerprintNormalizesLanguage(t *testing.T) {
	zhCN := configWith(t, 2, " zh-CN ")
	chinese := configWith(t, 2, "中文")
	en := configWith(t, 2, "en")
	if fingerprint(zhCN) != fingerprint(chinese) {
		t.Fatal("equivalent Chinese language aliases produced different fingerprints")
	}
	if fingerprint(zhCN) == fingerprint(en) {
		t.Fatal("different normalized languages produced the same fingerprint")
	}
}
