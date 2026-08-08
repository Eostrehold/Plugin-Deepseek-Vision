package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

func TestRawAnalyzerErrorIsNeverExposed(t *testing.T) {
	analyzer := &testAnalyzer{err: errors.New("provider secret token and private upstream body")}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/private.png"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || !resp.Terminate || resp.StatusCode != 502 {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	encoded := string(resp.ResponseBody)
	for _, forbidden := range []string{"provider secret", "private upstream", "private.png"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"category":"host_executor_error"`) {
		t.Fatalf("safe category missing: %s", encoded)
	}
}

type failureAnalyzer struct {
	failure *vision.Failure
}

func (a *failureAnalyzer) Analyze(context.Context, string, string) (string, error) {
	return "", a.failure
}

func TestStructuredVisionFailureContractsAndDiagnosticCorrelation(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		path        string
		body        string
		claudeShape bool
	}{
		{name: "responses", source: "openai-response", path: "/v1/responses", body: `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/secret-image.png"}]}]}`},
		{name: "chat", source: "openai", path: "/v1/chat/completions", body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/secret-image.png"}}]}]}`},
		{name: "claude", source: "claude", path: "/v1/messages", body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/secret-image.png"}}]}]}`, claudeShape: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := &vision.Failure{Code: "vision_fallback_exhausted", Attempts: []vision.AttemptFailure{
				{Model: "primary", Category: vision.FailureUpstreamHTTP, UpstreamStatus: 503, Retryable: true},
				{Model: "fallback", Category: vision.FailureInvalidResponse, Retryable: true},
			}}
			var diagnosticMessage string
			var diagnosticFields map[string]any
			r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return &failureAnalyzer{failure: failure}, nil }, func(_ string, _ string, message string, fields map[string]any) {
				diagnosticMessage, diagnosticFields = message, fields
			})
			r.Reconfigure(testConfig(t))
			defer r.Shutdown()
			resp, err := r.Handle(makeRequest("deepseek-v4-flash", tt.source, tt.path, tt.body))
			if err != nil || !resp.Terminate || resp.StatusCode != 502 {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
			encoded := string(resp.ResponseBody)
			for _, forbidden := range []string{"secret-image", "provider secret", "authorization", "api-key"} {
				if strings.Contains(strings.ToLower(encoded), forbidden) {
					t.Fatalf("response leaked %q: %s", forbidden, encoded)
				}
			}
			var root map[string]any
			if json.Unmarshal(resp.ResponseBody, &root) != nil {
				t.Fatalf("invalid JSON: %s", resp.ResponseBody)
			}
			errorObject := root["error"].(map[string]any)
			if tt.claudeShape {
				if root["type"] != "error" || errorObject["type"] != "api_error" {
					t.Fatalf("Claude shape=%#v", root)
				}
			} else if errorObject["type"] != "vision_preprocess_error" {
				t.Fatalf("OpenAI shape=%#v", root)
			}
			if errorObject["code"] != "vision_fallback_exhausted" || errorObject["message"] != "vision preprocessing failed" {
				t.Fatalf("error=%#v", errorObject)
			}
			details := errorObject["details"].(map[string]any)
			errorID, _ := details["error_id"].(string)
			attempts, _ := details["attempts"].([]any)
			if !strings.HasPrefix(errorID, "vpe_") || len(attempts) != 2 {
				t.Fatalf("details=%#v", details)
			}
			if diagnosticFields["error_id"] != errorID || !strings.Contains(diagnosticMessage, errorID) {
				t.Fatalf("diagnostic message=%q fields=%#v error_id=%q", diagnosticMessage, diagnosticFields, errorID)
			}
			diagnosticJSON, _ := json.Marshal(diagnosticFields)
			if strings.Contains(string(diagnosticJSON), "secret-image") || strings.Contains(diagnosticMessage, "secret-image") {
				t.Fatalf("diagnostic leaked request data: %s %s", diagnosticMessage, diagnosticJSON)
			}
		})
	}
}

type orderedFailureAnalyzer struct {
	mu sync.Mutex
}

func (a *orderedFailureAnalyzer) Analyze(ctx context.Context, reference, prompt string) (string, error) {
	if prompt == "first" {
		time.Sleep(20 * time.Millisecond)
		return "", &vision.Failure{Code: "vision_fallback_exhausted", Attempts: []vision.AttemptFailure{{Model: "job-one", Category: vision.FailureInvalidResponse, Retryable: true}}}
	}
	return "", &vision.Failure{Code: "vision_fallback_exhausted", Attempts: []vision.AttemptFailure{{Model: "job-two", Category: vision.FailureUpstreamHTTP, UpstreamStatus: 503, Retryable: true}}}
}

func TestConcurrentFailuresSelectLowestJobID(t *testing.T) {
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return &orderedFailureAnalyzer{}, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	body := `{"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"https://example.com/one.png"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"second"},{"type":"input_image","image_url":"https://example.com/two.png"}]}` +
		`]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || !resp.Terminate {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	if !strings.Contains(string(resp.ResponseBody), `"model":"job-one"`) || strings.Contains(string(resp.ResponseBody), `"model":"job-two"`) {
		t.Fatalf("nondeterministic public error: %s", resp.ResponseBody)
	}
}
