//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestRealGatewayVisionAndFlash is an opt-in smoke test for the development
// gateway. It deliberately does not persist or print the API key or any
// gateway response body. Set DEEPSEEK_VISION_API_KEY to enable it.
func TestRealGatewayVisionAndFlash(t *testing.T) {
	key := strings.TrimSpace(getenv("DEEPSEEK_VISION_API_KEY"))
	if key == "" {
		t.Skip("DEEPSEEK_VISION_API_KEY is not set")
	}

	baseURL := strings.TrimRight(getenv("DEEPSEEK_VISION_BASE_URL"), "/")
	if baseURL == "" {
		t.Skip("DEEPSEEK_VISION_BASE_URL is not set")
	}
	visionModel := defaultEnv("DEEPSEEK_VISION_MODEL", "gpt-5.6-luna")
	// Keep the target gate fixed to the known text-capable development model;
	// only the gateway base URL and VLM model are overrideable.
	const targetModel = "deepseek-v4-flash"

	// Keep all requests bounded. The plugin itself applies its own request and
	// per-call deadlines; this outer context bounds test cleanup as well.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	configureRealGateway(t, baseURL, visionModel)
	defer shutdownPlugin()

	imageRef := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG())
	body := responsesBody(imageRef, false)

	start := time.Now()
	rewritten := interceptAfter(t, pluginapi.RequestInterceptRequest{
		RequestID:      "integration-vision",
		SourceFormat:   "openai-response",
		ToFormat:       "openai-response",
		Model:          targetModel,
		RequestedModel: targetModel,
		Body:           []byte(body),
		Metadata:       map[string]any{"request_path": "/v1/responses"},
	})
	t.Logf("VLM rewrite model=%s status=ok elapsed=%s", visionModel, time.Since(start).Round(time.Millisecond))
	assertRewritten(t, rewritten, false)

	// stream=true must still be preprocessed before the upstream stream starts.
	streamBody := responsesBody(imageRef, true)
	streamRewritten := interceptAfter(t, pluginapi.RequestInterceptRequest{
		RequestID:      "integration-vision-stream",
		SourceFormat:   "openai-response",
		ToFormat:       "openai-response",
		Model:          targetModel,
		RequestedModel: targetModel,
		Stream:         true,
		Body:           []byte(streamBody),
		Metadata:       map[string]any{"request_path": "/v1/responses"},
	})
	assertRewritten(t, streamRewritten, true)

	// Exercise the real text-only target after image preprocessing. This is
	// intentionally separate from the plugin callback: it models the executor
	// receiving the rewritten body and proves that flash accepts the result.
	start = time.Now()
	status, responseBody := gatewayResponses(ctx, baseURL, key, targetModel, rewritten.Body, false)
	t.Logf("executor model=%s status=%d body_bytes=%d elapsed=%s", targetModel, status, len(responseBody), time.Since(start).Round(time.Millisecond))
	if status < 200 || status >= 300 {
		t.Fatalf("text-only %s request returned status %d", targetModel, status)
	}
	if !responseHasText(responseBody) {
		t.Fatalf("text-only %s response did not contain text", targetModel)
	}

}

func configureRealGateway(t *testing.T, baseURL, model string) {
	t.Helper()
	configYAML := fmt.Sprintf(`target_models: [deepseek-v4-flash]
vision_base_url: %s
vision_model: %s
vision_api_key_env: DEEPSEEK_VISION_API_KEY
language: en
request_timeout_seconds: 45
per_call_timeout_seconds: 30
retry_max_attempts: 1
max_concurrency: 2
max_images_per_request: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
cache_size: 8
cache_ttl_seconds: 60
`, yamlQuote(baseURL), yamlQuote(model))
	rawRequest, err := json.Marshal(map[string]any{"schema_version": 2, "config_yaml": []byte(configYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod("plugin.reconfigure", rawRequest); err != nil {
		t.Fatalf("configure plugin: %v", err)
	}
}

func interceptAfter(t *testing.T, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	t.Helper()
	rawRequest, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := handleMethod("request.intercept_after", rawRequest)
	if err != nil {
		t.Fatalf("after-auth interception: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("after-auth returned an error envelope")
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func assertRewritten(t *testing.T, response pluginapi.RequestInterceptResponse, stream bool) {
	t.Helper()
	if response.Terminate {
		t.Fatalf("vision preprocessing terminated request with status %d", response.StatusCode)
	}
	if len(response.Body) == 0 || bytes.Contains(response.Body, []byte(`input_image`)) || bytes.Contains(response.Body, []byte(`data:image/`)) {
		t.Fatalf("rewritten body is empty or retains an image (%d bytes)", len(response.Body))
	}
	var decoded struct {
		Stream bool `json:"stream"`
		Input  []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if decoded.Stream != stream || len(decoded.Input) != 1 || len(decoded.Input[0].Content) != 1 {
		t.Fatalf("unexpected rewritten shape: stream=%v inputs=%d content=%d body_bytes=%d", decoded.Stream, len(decoded.Input), len(decoded.Input[0].Content), len(response.Body))
	}
	block := decoded.Input[0].Content[0]
	if block.Type != "input_text" || strings.TrimSpace(block.Text) == "" || !strings.Contains(block.Text, "[Image 1 — Visual analysis]") || !strings.Contains(block.Text, "Visual description:") {
		t.Fatalf("unexpected visual replacement block: type=%q text_bytes=%d", block.Type, len(block.Text))
	}
}

func gatewayResponses(ctx context.Context, baseURL, key, model string, body []byte, stream bool) (int, []byte) {
	endpoint := strings.TrimRight(baseURL, "/") + "/responses"
	reqBody := append([]byte(nil), body...)
	if stream {
		var object map[string]any
		if json.Unmarshal(reqBody, &object) == nil {
			object["stream"] = true
			if encoded, err := json.Marshal(object); err == nil {
				reqBody = encoded
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects disabled") }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, data
}

func responseHasText(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(node any) bool {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "output_text" || key == "text" {
					if text, ok := child.(string); ok && strings.TrimSpace(text) != "" {
						return true
					}
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func responsesBody(imageRef string, stream bool) string {
	encoded, _ := json.Marshal(map[string]any{
		"model":  "deepseek-v4-flash",
		"input":  []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": imageRef}}}},
		"stream": stream,
	})
	return string(encoded)
}

func tinyPNG() []byte {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	return data
}

func yamlQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func defaultEnv(name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Kept as a variable to make the test's environment dependency explicit and
// easy to stub in package-level tests without ever writing credentials.
var getenv = func(name string) string {
	return strings.TrimSpace(lookupEnv(name))
}

var lookupEnv = os.Getenv
