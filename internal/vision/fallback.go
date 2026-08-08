package vision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
)

// FailureCategory is a stable, content-free vision attempt outcome.
type FailureCategory string

const (
	FailureHostExecutor     FailureCategory = "host_executor_error"
	FailureAttemptTimeout   FailureCategory = "attempt_timeout"
	FailureUpstreamHTTP     FailureCategory = "upstream_http"
	FailureInvalidResponse  FailureCategory = "invalid_response"
	FailureEmptyResponse    FailureCategory = "empty_response"
	FailureResponseTooLarge FailureCategory = "response_too_large"
	FailureResultTooLarge   FailureCategory = "result_too_large"
	FailureRequestTimeout   FailureCategory = "request_timeout"
	FailureRewriteFailed    FailureCategory = "rewrite_failed"
)

// AttemptFailure is safe to expose because it contains no provider response
// body, image reference, prompt, header, or credential.
type AttemptFailure struct {
	Model          string          `json:"model"`
	Category       FailureCategory `json:"category"`
	UpstreamStatus int             `json:"upstream_status,omitempty"`
	Retryable      bool            `json:"retryable"`
}

// Failure reports the ordered attempts made for one prompt group.
type Failure struct {
	Code     string           `json:"-"`
	Attempts []AttemptFailure `json:"attempts"`
	cause    error
}

func (e *Failure) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "vision preprocessing failed"
}

func (e *Failure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// FallbackOptions configures an ordered primary-plus-fallback model chain.
type FallbackOptions struct {
	Models           []string
	MaxResponseBytes int64
	MaxResultChars   int
	Language         string
	Execute          HostExecuteFunc
}

// FallbackAnalyzer attempts configured host models in order within the parent
// request deadline. It retains provider transport and credential ownership in
// CLIProxyAPI through HostClient.
type FallbackAnalyzer struct {
	models         []string
	clients        []*HostClient
	maxResultChars int
}

func NewFallbackAnalyzer(opts FallbackOptions) (*FallbackAnalyzer, error) {
	if len(opts.Models) == 0 {
		return nil, errors.New("vision model chain is empty")
	}
	chain := &FallbackAnalyzer{maxResultChars: opts.MaxResultChars}
	for _, model := range opts.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, errors.New("vision model chain contains an empty model")
		}
		client, err := NewHostClient(HostOptions{
			Model: model, MaxResponseBytes: opts.MaxResponseBytes,
			Language: opts.Language, Execute: opts.Execute,
		})
		if err != nil {
			return nil, err
		}
		chain.models = append(chain.models, model)
		chain.clients = append(chain.clients, client)
	}
	return chain, nil
}

func (a *FallbackAnalyzer) Analyze(ctx context.Context, imageReference, focusHint string) (string, error) {
	return a.AnalyzeBatch(ctx, []ImageInput{{Number: 1, Reference: imageReference}}, focusHint)
}

func (a *FallbackAnalyzer) AnalyzeBatch(ctx context.Context, images []ImageInput, prompt string) (string, error) {
	if a == nil || len(a.clients) == 0 {
		return "", &Failure{Code: "vision_fallback_exhausted", Attempts: []AttemptFailure{{Category: FailureHostExecutor, Retryable: true}}, cause: ErrHostExecutorUnavailable}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := make([]AttemptFailure, 0, len(a.clients))
	var lastErr error
	for i, client := range a.clients {
		if err := context.Cause(ctx); err != nil {
			attempts = append(attempts, AttemptFailure{Model: a.models[i], Category: FailureRequestTimeout, Retryable: false})
			return "", &Failure{Code: "vision_fallback_exhausted", Attempts: attempts, cause: err}
		}
		attemptCtx, cancel := fallbackAttemptContext(ctx, len(a.clients)-i)
		traceJob := tracelog.JobFromContext(attemptCtx)
		traceJob.Attempt = i + 1
		traceJob.ImageNumbers = make([]int, len(images))
		for imageIndex := range images {
			traceJob.ImageNumbers[imageIndex] = images[imageIndex].Number
		}
		attemptCtx = tracelog.WithJob(attemptCtx, traceJob)
		result, err := client.AnalyzeBatch(attemptCtx, images, prompt)
		attemptTimedOut := errors.Is(context.Cause(attemptCtx), context.DeadlineExceeded) && context.Cause(ctx) == nil
		cancel()
		if parentErr := context.Cause(ctx); parentErr != nil {
			attempts = append(attempts, AttemptFailure{Model: a.models[i], Category: FailureRequestTimeout, Retryable: false})
			return "", &Failure{Code: "vision_fallback_exhausted", Attempts: attempts, cause: parentErr}
		}
		if attemptTimedOut {
			err = context.DeadlineExceeded
		}
		if err == nil && a.maxResultChars > 0 && len([]rune(result)) > a.maxResultChars {
			err = ErrResultTooLarge
		}
		if err == nil {
			if session := tracelog.FromContext(ctx); session != nil {
				session.Event("vlm_fallback_selected", map[string]any{
					"job_id": traceJob.ID, "image_numbers": traceJob.ImageNumbers,
					"model": a.models[i], "attempt": i + 1,
				})
			}
			return result, nil
		}
		lastErr = err
		attempt := classifyAttempt(a.models[i], err, attemptTimedOut)
		attempts = append(attempts, attempt)
		if session := tracelog.FromContext(ctx); session != nil {
			session.Event("vlm_fallback_attempt", map[string]any{
				"job_id": traceJob.ID, "image_numbers": traceJob.ImageNumbers,
				"model": attempt.Model, "attempt": i + 1, "category": attempt.Category,
				"upstream_status": attempt.UpstreamStatus, "retryable": attempt.Retryable,
			})
		}
		if !attempt.Retryable {
			break
		}
	}
	return "", &Failure{Code: "vision_fallback_exhausted", Attempts: attempts, cause: lastErr}
}

func fallbackAttemptContext(parent context.Context, candidatesLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := parent.Deadline()
	if !ok || candidatesLeft <= 1 {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, remaining/time.Duration(candidatesLeft))
}

func classifyAttempt(model string, err error, attemptTimedOut bool) AttemptFailure {
	attempt := AttemptFailure{Model: model, Retryable: true}
	if attemptTimedOut {
		attempt.Category = FailureAttemptTimeout
		return attempt
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		attempt.Category = FailureUpstreamHTTP
		attempt.UpstreamStatus = status.StatusCode
		attempt.Retryable = status.StatusCode == http.StatusRequestTimeout || status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= 500
		return attempt
	}
	switch {
	case errors.Is(err, ErrInvalidResponse):
		attempt.Category = FailureInvalidResponse
	case errors.Is(err, ErrEmptyResponse):
		attempt.Category = FailureEmptyResponse
	case errors.Is(err, ErrResponseTooLarge):
		attempt.Category = FailureResponseTooLarge
	case errors.Is(err, ErrResultTooLarge):
		attempt.Category = FailureResultTooLarge
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		attempt.Category = FailureRequestTimeout
		attempt.Retryable = false
	default:
		attempt.Category = FailureHostExecutor
	}
	return attempt
}

func (a AttemptFailure) String() string {
	if a.UpstreamStatus > 0 {
		return fmt.Sprintf("%s:%s:%d", a.Model, a.Category, a.UpstreamStatus)
	}
	return fmt.Sprintf("%s:%s", a.Model, a.Category)
}
