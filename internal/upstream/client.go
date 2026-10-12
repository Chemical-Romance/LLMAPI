package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"llm-api/internal/domain"
)

// Client performs upstream OpenAI-compatible HTTP calls for one channel+key.
// HTTP clients are cached per timeout so connections are pooled and reused.
type Client struct {
	mu             sync.Mutex
	defaultTimeout time.Duration
	clients        map[time.Duration]*http.Client
}

func NewClient(defaultTimeout time.Duration) *Client {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	return &Client{
		defaultTimeout: defaultTimeout,
		clients:        map[time.Duration]*http.Client{},
	}
}

func (c *Client) timeoutFor(ch *domain.Channel) time.Duration {
	if ch.TimeoutSec > 0 {
		return time.Duration(ch.TimeoutSec) * time.Second
	}
	return c.defaultTimeout
}

func (c *Client) clientFor(d time.Duration) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[d]; ok {
		return cl
	}
	cl := &http.Client{Timeout: d}
	c.clients[d] = cl
	return cl
}

// Do performs a POST to channel.BaseURL+endpoint with the given key.
// The caller is responsible for closing resp.Body.
func (c *Client) Do(ctx context.Context, ch *domain.Channel, key, endpoint string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(ch.BaseURL, "/")
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	url := base + endpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch ch.AuthType {
	case domain.AuthTypeNone:
		// no auth header
	case domain.AuthTypeHeader:
		name := ch.APIKeyHeader
		if name == "" {
			name = "x-api-key"
		}
		req.Header.Set(name, key)
	default: // bearer
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := c.clientFor(c.timeoutFor(ch)).Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Usage is a parsed token usage summary. All fields optional (may be 0/absent).
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// ParseChatUsage extracts usage from a chat/completions response body (stream or not).
func ParseChatUsage(body []byte) Usage {
	var raw struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Usage == nil {
		return Usage{}
	}
	u := Usage{
		PromptTokens:     raw.Usage.PromptTokens,
		CompletionTokens: raw.Usage.CompletionTokens,
		TotalTokens:      raw.Usage.TotalTokens,
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

// CountImages returns the number of generated images in an images response.
func CountImages(body []byte) int {
	var raw struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0
	}
	return len(raw.Data)
}

// RewriteModel replaces the "model" field in a request body.
func RewriteModel(body []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureStreamUsageOption injects stream_options.include_usage for streaming
// calls so that the upstream returns a usage chunk we can settle with.
func EnsureStreamUsageOption(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	if _, ok := so["include_usage"]; !ok {
		so["include_usage"] = true
	}
	m["stream_options"] = so
	return json.Marshal(m)
}

// EstimatePromptTokens provides a rough prompt token estimate from a chat request body.
// Used to settle streaming calls where the upstream omits usage.
func EstimatePromptTokens(body []byte) int64 {
	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}
	var chars int
	for _, m := range req.Messages {
		switch v := m.Content.(type) {
		case string:
			chars += len([]rune(v))
		case []any:
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if txt, ok := pm["text"].(string); ok {
						chars += len([]rune(txt))
					}
				}
			}
		}
	}
	return int64(chars/4) + 4 // rough heuristic: ~4 chars per token
}

// EstimateCompletionTokens estimates completion tokens from streamed content runes.
func EstimateCompletionTokens(text string) int64 {
	chars := len([]rune(text))
	t := int64(chars / 4)
	if t < 1 {
		t = 1
	}
	return t
}

// ReadAll is a convenience wrapper.
func ReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
