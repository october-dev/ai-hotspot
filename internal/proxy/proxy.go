// Package proxy resolves model prefixes to upstreams and forwards OpenAI-compatible
// requests, capturing token usage from streaming and non-streaming responses.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/harshsaver/ai-hotspot/internal/config"
)

// Route is a resolved forwarding target: an upstream and the model to send to it.
type Route struct {
	Upstream *config.Upstream
	Model    string
}

// Usage holds token counts extracted from an upstream response.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

type usageJSON struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (u usageJSON) toUsage() Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// Proxy forwards requests to configured upstreams.
type Proxy struct {
	cfg    *config.Config
	client *http.Client
}

// New creates a Proxy. The HTTP client has no overall timeout so that long-lived
// streaming responses are not cut off; per-request cancellation flows from the
// incoming request's context instead.
func New(cfg *config.Config) *Proxy {
	return &Proxy{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// Resolve picks an upstream from a model string. A prefix matching a known
// upstream name (e.g. "openrouter/anthropic/claude-3.5-sonnet") selects that
// upstream and strips the prefix; otherwise the default upstream is used and the
// model is passed through unchanged.
func Resolve(model string, cfg *config.Config) (Route, bool) {
	if i := strings.Index(model, "/"); i > 0 {
		prefix := model[:i]
		if up := cfg.Upstream(prefix); up != nil {
			return Route{Upstream: up, Model: model[i+1:]}, true
		}
	}
	up := cfg.Upstream(cfg.DefaultUpstream)
	if up == nil {
		return Route{}, false
	}
	return Route{Upstream: up, Model: model}, true
}

// Prepare parses a chat-completions body, resolves the route, rewrites the model
// to strip any upstream prefix, and (for streaming requests) injects
// stream_options.include_usage so usage tokens arrive in the final chunk. It
// returns a possibly-rewritten body ready to forward.
func (p *Proxy) Prepare(body []byte) (route Route, streaming bool, out []byte, err error) {
	var m map[string]json.RawMessage
	if err = json.Unmarshal(body, &m); err != nil {
		return Route{}, false, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	var model string
	if raw, ok := m["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if raw, ok := m["stream"]; ok {
		_ = json.Unmarshal(raw, &streaming)
	}
	route, ok := Resolve(model, p.cfg)
	if !ok {
		return Route{}, false, nil, fmt.Errorf("no upstream available for model %q", model)
	}

	modified := false
	if route.Model != model {
		mb, _ := json.Marshal(route.Model)
		m["model"] = mb
		modified = true
	}
	if streaming {
		if _, ok := m["stream_options"]; !ok {
			m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
			modified = true
		}
	}
	if !modified {
		return route, streaming, body, nil
	}
	out, err = json.Marshal(m)
	if err != nil {
		return Route{}, false, nil, err
	}
	return route, streaming, out, nil
}

func (p *Proxy) do(ctx context.Context, route Route, pathSuffix, method string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(route.Upstream.BaseURL, "/") + pathSuffix
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer "+route.Upstream.APIKey)
	for k, v := range route.Upstream.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return p.client.Do(req)
}

// SendChat forwards a prepared chat-completions body to the route's upstream.
func (p *Proxy) SendChat(ctx context.Context, route Route, body []byte) (*http.Response, error) {
	return p.do(ctx, route, "/chat/completions", http.MethodPost, body)
}

// SendModels forwards a GET /models request to the default upstream.
func (p *Proxy) SendModels(ctx context.Context) (*http.Response, error) {
	up := p.cfg.Upstream(p.cfg.DefaultUpstream)
	if up == nil {
		return nil, fmt.Errorf("default upstream %q not configured", p.cfg.DefaultUpstream)
	}
	return p.do(ctx, Route{Upstream: up}, "/models", http.MethodGet, nil)
}

// Relay writes an upstream response to the client. For a successful streaming
// response it flushes SSE chunks as they arrive and captures usage from the final
// chunk; otherwise it buffers and copies the body, parsing usage from the JSON.
// It always closes resp.Body.
func Relay(w http.ResponseWriter, resp *http.Response, streaming bool) Usage {
	defer resp.Body.Close()
	if streaming && resp.StatusCode == http.StatusOK {
		return relayStream(w, resp)
	}
	return relayBuffered(w, resp)
}

func relayStream(w http.ResponseWriter, resp *http.Response) Usage {
	flusher, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}

	var usage Usage
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if u, ok := parseSSEUsage(line); ok {
				usage = u
			}
			if _, werr := w.Write(line); werr != nil {
				return usage
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return usage
		}
	}
}

func relayBuffered(w http.ResponseWriter, resp *http.Response) Usage {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return Usage{}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)

	var parsed struct {
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil && parsed.Usage != nil {
		return parsed.Usage.toUsage()
	}
	return Usage{}
}

// parseSSEUsage extracts a usage object from an SSE "data:" line, if present.
func parseSSEUsage(line []byte) (Usage, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return Usage{}, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return Usage{}, false
	}
	var chunk struct {
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return Usage{}, false
	}
	return chunk.Usage.toUsage(), true
}
