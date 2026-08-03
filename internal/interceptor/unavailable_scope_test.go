package interceptor

import "testing"

func TestRuntimeUnavailablePreservesTargetModelGate(t *testing.T) {
	r := newTestRuntime(t, &testAnalyzer{})
	r.Shutdown()

	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/history.png"}]}]}`
	cases := []struct {
		name       string
		model      string
		terminated bool
	}{
		{name: "configured deepseek target", model: "deepseek-v4-flash", terminated: true},
		{name: "configured second deepseek target", model: "deepseek-v4-pro", terminated: true},
		{name: "unrelated model", model: "gpt-5.6-luna", terminated: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := r.Handle(makeRequest(tc.model, "openai-response", "/v1/responses", body))
			if err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
			if resp.Terminate != tc.terminated {
				t.Fatalf("model=%q terminated=%v, want %v", tc.model, resp.Terminate, tc.terminated)
			}
			if !tc.terminated && string(resp.Body) != body {
				t.Fatalf("non-target body changed: %s", resp.Body)
			}
		})
	}
}

func TestHandleUnavailableUsesExplicitTargetModelGate(t *testing.T) {
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`
	resp, err := HandleUnavailable(makeRequest("gpt-5.6-luna", "openai-response", "/v1/responses", body), "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("HandleUnavailable returned error: %v", err)
	}
	if resp.Terminate {
		t.Fatalf("unrelated model was terminated: %#v", resp)
	}
	if string(resp.Body) != body {
		t.Fatalf("unrelated model body changed: %s", resp.Body)
	}
}

func TestResponsesOnlyScopePassesOtherSourceFormats(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()

	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`
	for _, source := range []string{"openai", "anthropic", "openai-request"} {
		t.Run(source, func(t *testing.T) {
			resp, err := r.Handle(makeRequest("deepseek-v4-flash", source, "/v1/responses", body))
			if err != nil || resp.Terminate || string(resp.Body) != body {
				t.Fatalf("source=%q was not byte-preserving passthrough: err=%v response=%#v", source, err, resp)
			}
		})
	}
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	if len(analyzer.refs) != 0 {
		t.Fatalf("unsupported source formats invoked analyzer: %v", analyzer.refs)
	}
}
