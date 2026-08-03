package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
)

var (
	ErrInvalidResponse  = errors.New("invalid visual model response")
	ErrEmptyResponse    = errors.New("visual model returned empty response")
	ErrResponseTooLarge = errors.New("visual model response exceeds size limit")
	ErrResponseRead     = errors.New("visual model response was interrupted")
	ErrClientClosed     = errors.New("visual model client is closed")
)

type Options struct {
	BaseURL                string
	Model                  string
	Token                  string
	RequestTimeout         time.Duration
	MaxResponseBytes       int64
	MaxResultChars         int
	MaxImageReferenceBytes int
	MaxAttempts            int
	RetryBaseDelay         time.Duration
	MaxRetryDelay          time.Duration
	HTTPClient             *http.Client
	ConfigGeneration       string
	Language               string
}

type Client struct {
	httpClient *http.Client
	endpoint   string
	opts       Options
	transport  *http.Transport
	closed     chan struct{}
	closeOnce  sync.Once
}

func NewClient(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("vision base URL is required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = "gpt-5.6-luna"
	}
	opts.Language = NormalizeLanguage(opts.Language)
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 120 * time.Second
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 4 << 20
	}
	if opts.MaxResultChars <= 0 {
		opts.MaxResultChars = 20000
	}
	if opts.MaxImageReferenceBytes <= 0 {
		opts.MaxImageReferenceBytes = 16 << 20
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = 100 * time.Millisecond
	}
	if opts.MaxRetryDelay <= 0 {
		opts.MaxRetryDelay = 2 * time.Second
	}
	u, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" || u.User != nil || u.Hostname() == "" || !validPort(u.Port()) || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("vision base URL must be an http(s) URL without credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/responses") {
		return nil, errors.New("vision base URL must not include /responses")
	}
	u.Path = path
	u.RawPath = ""
	endpoint := strings.TrimRight(u.String(), "/") + "/responses"

	var hc *http.Client
	var tr *http.Transport
	if opts.HTTPClient != nil {
		// Clone the client value before installing the redirect policy. The
		// caller may reuse its client elsewhere and must not observe mutation.
		clientClone := *opts.HTTPClient
		hc = &clientClone
	} else {
		base, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			tr = base.Clone()
		} else {
			tr = &http.Transport{}
		}
		tr.TLSHandshakeTimeout = 10 * time.Second
		tr.ResponseHeaderTimeout = opts.RequestTimeout
		tr.IdleConnTimeout = 90 * time.Second
		hc = &http.Client{Transport: tr, Timeout: opts.RequestTimeout}
	}
	// Never follow redirects for a request carrying an image reference and
	// bearer token. Returning the 3xx response keeps the body on the original
	// origin and prevents cross-host replay or HTTPS-to-HTTP credential leaks.
	hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{httpClient: hc, endpoint: endpoint, opts: opts, transport: tr, closed: make(chan struct{})}, nil
}

func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.closed)
		// Only the transport allocated by NewClient is owned here. A transport
		// supplied through Options.HTTPClient may be shared by unrelated users.
		if c.transport != nil {
			c.transport.CloseIdleConnections()
		}
	})
	return nil
}

func (c *Client) Analyze(ctx context.Context, imageReference, focusHint string) (string, error) {
	if c == nil {
		return "", ErrClientClosed
	}
	select {
	case <-c.closed:
		return "", ErrClientClosed
	default:
	}
	if err := safety.ValidateImageReference(imageReference, c.opts.MaxImageReferenceBytes); err != nil {
		return "", err
	}
	payload := requestPayload{
		Model: c.opts.Model,
		Input: []requestInput{{Role: "user", Content: []requestContent{
			{Type: "input_text", Text: BuildPrompt(focusHint, c.opts.Language)},
			{Type: "input_image", ImageURL: imageReference},
		}}},
		MaxOutputTokens: 4096,
		Stream:          false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	// RequestTimeout is a total deadline for all attempts, backoff included.
	// This prevents a sequence of transient failures from multiplying the
	// configured timeout by MaxAttempts.
	callCtx := ctx
	var cancel context.CancelFunc
	if c.opts.RequestTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}
	var lastErr error
	for attempt := 0; attempt < c.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.waitRetry(callCtx, attempt); err != nil {
				return "", err
			}
		}
		result, retry, err := c.doRequest(callCtx, body)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	if lastErr == nil {
		lastErr = ErrInvalidResponse
	}
	return "", lastErr
}

func (c *Client) waitRetry(ctx context.Context, attempt int) error {
	delay := c.opts.RetryBaseDelay
	for i := 1; i < attempt; i++ {
		if delay >= c.opts.MaxRetryDelay/2 {
			delay = c.opts.MaxRetryDelay
			break
		}
		delay *= 2
	}
	if delay > c.opts.MaxRetryDelay {
		delay = c.opts.MaxRetryDelay
	}
	// 50-100% jitter keeps simultaneous plugin requests from synchronizing.
	delay = time.Duration(float64(delay) * (0.5 + rand.Float64()/2))
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClientClosed
	case <-t.C:
		return nil
	}
}

func (c *Client) doRequest(ctx context.Context, body []byte) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		// Do not return the underlying error: net/http may include the endpoint
		// URL and transport details in it. The endpoint can contain sensitive
		// deployment information, so expose only a stable class.
		return "", isRetryableTransportError(err), errors.New("visual model request failed")
	}
	defer resp.Body.Close()
	data, readErr := readLimited(resp.Body, c.opts.MaxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status policy takes precedence over body transport errors. In
		// particular, a truncated 4xx response must never become retryable.
		return "", retryStatus(resp.StatusCode), fmt.Errorf("visual model returned status %d", resp.StatusCode)
	}
	if readErr != nil {
		if errors.Is(readErr, ErrResponseTooLarge) {
			return "", false, readErr
		}
		return "", isRetryableTransportError(readErr), ErrResponseRead
	}
	text, err := parseText(data)
	if err != nil {
		return "", false, err
	}
	if len([]rune(text)) > c.opts.MaxResultChars {
		return "", false, ErrResponseTooLarge
	}
	return text, false, nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 4 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func retryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func isRetryableTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return isRetryableTransportError(urlErr.Err)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() || ne.Temporary() {
			return true
		}
	}
	// net.OpError covers connection refused/reset/broken-pipe failures whose
	// Temporary method is false on modern Go. They are safe to retry because
	// every attempt recreates the idempotent VLM request body.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

type requestPayload struct {
	Model           string         `json:"model"`
	Input           []requestInput `json:"input"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	Stream          bool           `json:"stream"`
}
type requestInput struct {
	Role    string           `json:"role"`
	Content []requestContent `json:"content"`
}
type requestContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

func parseText(data []byte) (string, error) {
	var raw struct {
		OutputText string            `json:"output_text"`
		Output     []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", ErrInvalidResponse
	}
	if s := strings.TrimSpace(raw.OutputText); s != "" {
		return s, nil
	}
	var parts []string
	for _, item := range raw.Output {
		var msg struct {
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(item, &msg) != nil || msg.Type == "reasoning" {
			continue
		}
		for _, c := range msg.Content {
			var block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(c, &block) == nil && block.Type == "output_text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, strings.TrimSpace(block.Text))
			}
		}
	}
	if len(parts) == 0 {
		return "", ErrEmptyResponse
	}
	return strings.Join(parts, "\n"), nil
}
