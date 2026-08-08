package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
)

func TestHostClientUsesResponsesProtocolWithoutCredentials(t *testing.T) {
	var got pluginapi.HostModelExecutionRequest
	var callbackID string
	client, err := NewHostClient(HostOptions{
		Model:    "vision-model",
		Language: "zh",
		Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, id string) (pluginapi.HostModelExecutionResponse, error) {
			got, callbackID = request, id
			return pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"output_text":"Visible text: hello\nVisual description: screen"}`),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHostCallbackID(context.Background(), "callback-1")
	result, err := client.Analyze(ctx, "data:image/png;base64,AAAA", "read the screen")
	if err != nil {
		t.Fatal(err)
	}
	if result == "" || callbackID != "callback-1" {
		t.Fatalf("result=%q callbackID=%q", result, callbackID)
	}
	if got.EntryProtocol != "openai-response" || got.ExitProtocol != "openai-response" || got.Model != "vision-model" || got.Stream {
		t.Fatalf("host request = %#v", got)
	}
	if got.Headers.Get("Authorization") != "" || got.Headers.Get("Cookie") != "" || got.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("host headers = %#v", got.Headers)
	}
	var payload requestPayload
	if err := json.Unmarshal(got.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Model != "vision-model" || len(payload.Input) != 1 || len(payload.Input[0].Content) != 3 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.MaxOutputTokens != 2048 || payload.Reasoning == nil || payload.Reasoning.Effort != "low" || payload.Stream {
		t.Fatalf("latency controls = %#v", payload)
	}
	prompt := payload.Input[0].Content[0].Text
	for _, fragment := range []string{"Focus on visual facts", "credible corrective actions", "summarize repetitive or unrelated text", "Return concise plain text"} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("prompt missing %q: %s", fragment, prompt)
		}
	}
}

func TestHostClientSendsOneOrderedMultiImageRequest(t *testing.T) {
	var got pluginapi.HostModelExecutionRequest
	client, err := NewHostClient(HostOptions{Model: "vision-model", Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		got = request
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"joint"}`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AnalyzeBatch(context.Background(), []ImageInput{{Number: 2, Reference: "https://example.com/a.png"}, {Number: 4, Reference: "https://example.com/b.png"}}, "compare")
	if err != nil {
		t.Fatal(err)
	}
	var payload requestPayload
	if err := json.Unmarshal(got.Body, &payload); err != nil {
		t.Fatal(err)
	}
	content := payload.Input[0].Content
	if len(content) != 5 || content[1].Text != "Image 1:" || content[2].ImageURL != "https://example.com/a.png" || content[3].Text != "Image 2:" || content[4].ImageURL != "https://example.com/b.png" {
		t.Fatalf("ordered multi-image content = %#v", content)
	}
	if payload.MaxOutputTokens != 4096 || payload.Reasoning == nil || payload.Reasoning.Effort != "low" {
		t.Fatalf("multi-image latency controls = %#v", payload)
	}
}

func TestHostClientExposes413ForAdaptiveSplitting(t *testing.T) {
	client, err := NewHostClient(HostOptions{Execute: func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusRequestEntityTooLarge, Body: []byte(`{"error":"too large"}`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AnalyzeBatch(context.Background(), []ImageInput{{Number: 1, Reference: "https://example.com/a.png"}}, "")
	if !IsPayloadTooLarge(err) {
		t.Fatalf("error = %v", err)
	}
}

func TestHostClientTraceSeparatesAdaptiveSplitBatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(tracelog.RequestMeta{RequestID: "split-trace"})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	ctx := tracelog.WithSession(context.Background(), session)
	ctx = tracelog.WithJob(ctx, tracelog.Job{ID: 5, ImageNumbers: []int{1, 2}, Attempt: 1})

	client, err := NewHostClient(HostOptions{Model: "split-model", Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		var payload requestPayload
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatal(err)
		}
		images := 0
		for _, content := range payload.Input[0].Content {
			if content.Type == "input_image" {
				images++
			}
		}
		if images > 1 {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusRequestEntityTooLarge, Body: []byte(`{"error":"split required"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"single result"}`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	images := []ImageInput{{Number: 1, Reference: "https://example.com/1.png"}, {Number: 2, Reference: "https://example.com/2.png"}}
	if _, err := client.AnalyzeBatch(ctx, images, "split"); !IsPayloadTooLarge(err) {
		t.Fatalf("multi-image error = %v", err)
	}
	for i := range images {
		if _, err := client.AnalyzeBatch(ctx, images[i:i+1], "split"); err != nil {
			t.Fatalf("single image %d: %v", i+1, err)
		}
	}
	session.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	checks := map[string]string{
		"40-vlm-job-005-attempt-01-images-1-2-response.json":   "split required",
		"40-vlm-job-005-attempt-01-images-1-parsed-result.txt": "single result",
		"40-vlm-job-005-attempt-01-images-2-parsed-result.txt": "single result",
	}
	for name, fragment := range checks {
		raw, readErr := os.ReadFile(filepath.Join(bundle, name))
		if readErr != nil || !strings.Contains(string(raw), fragment) {
			t.Fatalf("artifact %s: err=%v body=%s", name, readErr, raw)
		}
	}
}

func TestHostClientRetriesWithoutReasoningAfter400(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(tracelog.RequestMeta{RequestID: "reasoning-compat"})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	ctx := tracelog.WithSession(context.Background(), session)
	ctx = tracelog.WithJob(ctx, tracelog.Job{ID: 3, ImageNumbers: []int{1}, Attempt: 1})

	calls := 0
	client, err := NewHostClient(HostOptions{Model: "compat-model", Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		calls++
		var payload requestPayload
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if payload.Reasoning == nil || payload.Reasoning.Effort != "low" {
				t.Fatalf("first payload reasoning = %#v", payload.Reasoning)
			}
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"unsupported optional field"}`)}, nil
		}
		if payload.Reasoning != nil {
			t.Fatalf("fallback payload reasoning = %#v", payload.Reasoning)
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"compatible"}`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Analyze(ctx, "https://example.com/image.png", "describe")
	if err != nil || result != "compatible" || calls != 2 {
		t.Fatalf("result=%q calls=%d err=%v", result, calls, err)
	}
	session.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	prefix := "40-vlm-job-003-attempt-01-images-1"
	checks := map[string][]string{
		prefix + "-request.json":                     {`"reasoning":{"effort":"low"}`},
		prefix + "-reasoning-rejected-response.json": {"unsupported optional field"},
		prefix + "-reasoning-fallback-request.json":  {`"model":"compat-model"`},
		prefix + "-response.json":                    {"compatible"},
		prefix + "-parsed-result.txt":                {"compatible"},
	}
	for name, fragments := range checks {
		raw, readErr := os.ReadFile(filepath.Join(bundle, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Fatalf("%s missing %q: %s", name, fragment, raw)
			}
		}
	}
	fallbackRequest, err := os.ReadFile(filepath.Join(bundle, prefix+"-reasoning-fallback-request.json"))
	if err != nil || strings.Contains(string(fallbackRequest), `"reasoning"`) {
		t.Fatalf("reasoning compatibility trace retained rejected field: err=%v body=%s", err, fallbackRequest)
	}
}

func TestHostClientBoundsAndSanitizesFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		response pluginapi.HostModelExecutionResponse
	}{
		{name: "status", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`secret upstream text`)}},
		{name: "oversized", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"too large"}`)}},
		{name: "invalid", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`not-json`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			maxBytes := int64(1024)
			if test.name == "oversized" {
				maxBytes = 4
			}
			client, err := NewHostClient(HostOptions{
				Model:            "vision-model",
				MaxResponseBytes: maxBytes,
				Execute: func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error) {
					return test.response, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Analyze(context.Background(), "data:image/png;base64,AAAA", ""); err == nil {
				t.Fatal("host failure unexpectedly succeeded")
			}
		})
	}
}

func TestHostClientWritesFullPlaintextTrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(tracelog.RequestMeta{RequestID: "request-vlm", TraceID: "trace-vlm", ConfigGeneration: 2})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	ctx := tracelog.WithSession(context.Background(), session)
	ctx = tracelog.WithJob(ctx, tracelog.Job{ID: 1, ImageNumbers: []int{2, 4}})
	ctx = WithHostCallbackID(ctx, "callback-vlm")

	responseBody := []byte(`{"output_text":"Visible text: plaintext response\nVisual description: traced"}`)
	client, err := NewHostClient(HostOptions{
		Model: "vision-model", Language: "zh",
		Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, callbackID string) (pluginapi.HostModelExecutionResponse, error) {
			if callbackID != "callback-vlm" || !strings.Contains(string(request.Body), "data:image/png;base64,PLAINTEXT") {
				t.Fatalf("request=%s callback=%q", request.Body, callbackID)
			}
			return pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Set-Cookie": []string{"provider-secret"}, "X-Api_Key": []string{"header-api-key"}, "X-Trace": []string{"visible"}},
				Body:       responseBody,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Analyze(ctx, "data:image/png;base64,PLAINTEXT", "full focus hint")
	if err != nil || !strings.Contains(result, "plaintext response") {
		t.Fatalf("result=%q err=%v", result, err)
	}
	session.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	checks := map[string][]string{
		"40-vlm-job-001-attempt-01-images-1-metadata.json":     {"data:image/png;base64,PLAINTEXT", "full focus hint"},
		"40-vlm-job-001-attempt-01-images-1-request.json":      {"data:image/png;base64,PLAINTEXT"},
		"40-vlm-job-001-attempt-01-images-1-response.json":     {"plaintext response"},
		"40-vlm-job-001-attempt-01-images-1-parsed-result.txt": {"plaintext response"},
	}
	for name, fragments := range checks {
		raw, readErr := os.ReadFile(filepath.Join(bundle, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Fatalf("%s missing %q: %s", name, fragment, raw)
			}
		}
	}
	responseMetadata, err := os.ReadFile(filepath.Join(bundle, "40-vlm-job-001-attempt-01-images-1-response-metadata.json"))
	if err != nil || strings.Contains(string(responseMetadata), "provider-secret") || strings.Contains(string(responseMetadata), "header-api-key") || !strings.Contains(string(responseMetadata), "[REDACTED]") {
		t.Fatalf("response metadata redaction failed: err=%v body=%s", err, responseMetadata)
	}
}
