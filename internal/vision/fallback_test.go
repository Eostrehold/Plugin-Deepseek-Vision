package vision

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
)

func TestFallbackAnalyzerRetryableFailuresSelectNextModel(t *testing.T) {
	tests := []struct {
		name             string
		maxResponseBytes int64
		maxResultChars   int
		primary          func(context.Context) (pluginapi.HostModelExecutionResponse, error)
		category         FailureCategory
		status           int
	}{
		{name: "host executor", primary: func(context.Context) (pluginapi.HostModelExecutionResponse, error) {
			return pluginapi.HostModelExecutionResponse{}, errors.New("secret executor failure")
		}, category: FailureHostExecutor},
		{name: "408", primary: statusResponse(http.StatusRequestTimeout), category: FailureUpstreamHTTP, status: 408},
		{name: "429", primary: statusResponse(http.StatusTooManyRequests), category: FailureUpstreamHTTP, status: 429},
		{name: "503", primary: statusResponse(http.StatusServiceUnavailable), category: FailureUpstreamHTTP, status: 503},
		{name: "invalid status", primary: statusResponse(0), category: FailureInvalidResponse},
		{name: "invalid", primary: bodyResponse(http.StatusOK, []byte(`{`)), category: FailureInvalidResponse},
		{name: "empty", primary: bodyResponse(http.StatusOK, []byte(`{}`)), category: FailureEmptyResponse},
		{name: "response too large", maxResponseBytes: 24, primary: bodyResponse(http.StatusOK, []byte(`{"output_text":"this is a long response"}`)), category: FailureResponseTooLarge},
		{name: "result too large", maxResultChars: 2, primary: bodyResponse(http.StatusOK, []byte(`{"output_text":"long"}`)), category: FailureResultTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var models []string
			execute := func(ctx context.Context, req pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
				models = append(models, req.Model)
				if req.Model == "primary" {
					return tt.primary(ctx)
				}
				return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"ok"}`)}, nil
			}
			analyzer, err := NewFallbackAnalyzer(FallbackOptions{Models: []string{"primary", "secondary"}, Execute: execute, MaxResponseBytes: tt.maxResponseBytes, MaxResultChars: tt.maxResultChars})
			if err != nil {
				t.Fatal(err)
			}
			result, err := analyzer.Analyze(context.Background(), "data:image/png;base64,AAAA", "focus")
			if err != nil || result != "ok" {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if !reflect.DeepEqual(models, []string{"primary", "secondary"}) {
				t.Fatalf("models=%v", models)
			}
		})
	}
}

func TestFallbackAnalyzerTracePreservesEveryAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(tracelog.RequestMeta{RequestID: "fallback-trace"})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	ctx := tracelog.WithSession(context.Background(), session)
	ctx = tracelog.WithJob(ctx, tracelog.Job{ID: 7, ImageNumbers: []int{1}})

	analyzer, err := NewFallbackAnalyzer(FallbackOptions{
		Models: []string{"primary", "secondary"},
		Execute: func(_ context.Context, req pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
			if req.Model == "primary" {
				return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":"primary attempt body"}`)}, nil
			}
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"secondary result"}`)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := analyzer.Analyze(ctx, "data:image/png;base64,TRACE", "trace every model")
	if err != nil || result != "secondary result" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	session.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	checks := map[string]string{
		"40-vlm-job-007-attempt-01-images-1-response.json":          "primary attempt body",
		"40-vlm-job-007-attempt-01-images-1-response-metadata.json": `"model": "primary"`,
		"40-vlm-job-007-attempt-02-images-1-response.json":          "secondary result",
		"40-vlm-job-007-attempt-02-images-1-response-metadata.json": `"model": "secondary"`,
		"40-vlm-job-007-attempt-02-images-1-parsed-result.txt":      "secondary result",
	}
	for name, fragment := range checks {
		raw, readErr := os.ReadFile(filepath.Join(bundle, name))
		if readErr != nil || !strings.Contains(string(raw), fragment) {
			t.Fatalf("artifact %s: err=%v body=%s", name, readErr, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "40-vlm-job-007-attempt-01-images-1-parsed-result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed attempt unexpectedly wrote parsed result: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if err != nil || !strings.Contains(string(events), `"event":"vlm_fallback_attempt"`) ||
		!strings.Contains(string(events), `"event":"vlm_fallback_selected"`) ||
		!strings.Contains(string(events), `"job_id":7`) ||
		!strings.Contains(string(events), `"image_numbers":[1]`) {
		t.Fatalf("fallback event correlation missing: err=%v events=%s", err, events)
	}
}

func TestFallbackAnalyzerOrdinary400StopsAfterCompatibilityRetry(t *testing.T) {
	var models []string
	analyzer, err := NewFallbackAnalyzer(FallbackOptions{Models: []string{"primary", "secondary"}, Execute: func(_ context.Context, req pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		models = append(models, req.Model)
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadRequest, Body: []byte(`provider secret`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.Analyze(context.Background(), "data:image/png;base64,AAAA", "focus")
	var failure *Failure
	if !errors.As(err, &failure) || len(failure.Attempts) != 1 || failure.Attempts[0].Retryable || failure.Attempts[0].UpstreamStatus != 400 {
		t.Fatalf("failure=%#v err=%v", failure, err)
	}
	if !reflect.DeepEqual(models, []string{"primary", "primary"}) {
		t.Fatalf("models=%v", models)
	}
}

func TestFallbackAnalyzerAttemptTimeoutLeavesTimeForNextModel(t *testing.T) {
	var models []string
	analyzer, err := NewFallbackAnalyzer(FallbackOptions{Models: []string{"slow", "fast"}, Execute: func(ctx context.Context, req pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		models = append(models, req.Model)
		if req.Model == "slow" {
			<-ctx.Done()
			// A misbehaving executor may return a nominal success after ignoring the
			// attempt deadline; the fallback layer must still reject that result.
			return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"output_text":"late"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"output_text":"ok"}`)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := analyzer.Analyze(ctx, "data:image/png;base64,AAAA", "focus")
	if err != nil || result != "ok" || !reflect.DeepEqual(models, []string{"slow", "fast"}) {
		t.Fatalf("result=%q models=%v err=%v", result, models, err)
	}
}

func TestFallbackAnalyzerParentCancellationStopsImmediately(t *testing.T) {
	calls := 0
	analyzer, err := NewFallbackAnalyzer(FallbackOptions{Models: []string{"primary", "secondary"}, Execute: func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error) {
		calls++
		return pluginapi.HostModelExecutionResponse{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = analyzer.Analyze(ctx, "data:image/png;base64,AAAA", "focus")
	var failure *Failure
	if !errors.As(err, &failure) || len(failure.Attempts) != 1 || failure.Attempts[0].Category != FailureRequestTimeout || calls != 0 {
		t.Fatalf("failure=%#v calls=%d err=%v", failure, calls, err)
	}
}

func TestFallbackFailureUnwrapsPayloadTooLarge(t *testing.T) {
	analyzer, err := NewFallbackAnalyzer(FallbackOptions{Models: []string{"primary", "secondary"}, Execute: func(_ context.Context, req pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusRequestEntityTooLarge}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = analyzer.Analyze(context.Background(), "data:image/png;base64,AAAA", "focus")
	if !IsPayloadTooLarge(err) {
		t.Fatalf("expected wrapped 413, got %v", err)
	}
}

func statusResponse(status int) func(context.Context) (pluginapi.HostModelExecutionResponse, error) {
	return func(context.Context) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: status}, nil
	}
}

func bodyResponse(status int, body []byte) func(context.Context) (pluginapi.HostModelExecutionResponse, error) {
	return func(context.Context) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: status, Body: body}, nil
	}
}
