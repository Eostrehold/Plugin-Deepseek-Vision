package interceptor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

func TestHandleChatRewritesImagesForStreamingAndNonStreaming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			analyzer := &batchTestAnalyzer{}
			r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
			r.Reconfigure(testConfig(t))
			defer r.Shutdown()
			body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"compare"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]}]}`
			req := makeRequest("deepseek-v4-flash", "openai", "/v1/chat/completions", body)
			req.Stream = stream
			resp, err := r.Handle(req)
			if err != nil || resp.Terminate {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
			rewritten := string(resp.Body)
			if strings.Contains(rewritten, "image_url") || strings.Contains(rewritten, "example.com") || !strings.Contains(rewritten, "Joint visual analysis") {
				t.Fatalf("chat rewrite=%s", rewritten)
			}
			analyzer.mu.Lock()
			batches := len(analyzer.batches)
			images := 0
			if batches > 0 {
				images = len(analyzer.batches[0])
			}
			analyzer.mu.Unlock()
			if batches != 1 || images != 2 {
				t.Fatalf("batches=%d images=%d", batches, images)
			}
		})
	}
}

func TestChatRouteGateAndErrors(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	for _, tc := range []struct {
		name   string
		source string
		path   string
		model  string
		pass   bool
	}{
		{name: "exact", source: "openai", path: "/v1/chat/completions", model: "deepseek-v4-flash"},
		{name: "wrong path", source: "openai", path: "/v1/completions", model: "deepseek-v4-flash", pass: true},
		{name: "wrong source", source: "openai-response", path: "/v1/chat/completions", model: "deepseek-v4-flash", pass: true},
		{name: "other model", source: "openai", path: "/v1/chat/completions", model: "other", pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRuntime(t, &testAnalyzer{})
			defer r.Shutdown()
			resp, err := r.Handle(makeRequest(tc.model, tc.source, tc.path, body))
			if err != nil || resp.Terminate {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
			if gotPass := string(resp.Body) == body; gotPass != tc.pass {
				t.Fatalf("passthrough=%v want=%v body=%s", gotPass, tc.pass, resp.Body)
			}
		})
	}

	r := newTestRuntime(t, &testAnalyzer{})
	defer r.Shutdown()
	malformed, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai", "/v1/chat/completions", `{`))
	if !malformed.Terminate || malformed.StatusCode != http.StatusBadRequest || !strings.Contains(string(malformed.ResponseBody), `"error"`) || strings.Contains(string(malformed.ResponseBody), `"type":"error"`) {
		t.Fatalf("malformed response=%#v", malformed)
	}
	unsupportedBody := `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1/a.png"}}]}]}`
	unsupported, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai", "/v1/chat/completions", unsupportedBody))
	if !unsupported.Terminate || unsupported.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported response=%#v", unsupported)
	}
}

func TestUnavailableChatImageFailsClosed(t *testing.T) {
	body := `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	resp, err := HandleUnavailable(makeRequest("deepseek-v4-flash", "openai", "/v1/chat/completions", body), "deepseek-v4-flash")
	if err != nil || !resp.Terminate || resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(resp.ResponseBody), "vision preprocessing is unavailable") {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
}
