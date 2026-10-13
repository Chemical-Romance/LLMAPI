package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-api/internal/config"
	"llm-api/internal/domain"
	"llm-api/internal/route"
	"llm-api/internal/store"
	"llm-api/internal/upstream"
)

const (
	maxAttempts       = 8
	maxRequestBody    = 32 << 20 // 32MB
	maxUpstreamErrLen = 1024
)

// hopHeaders are connection-specific headers that a proxy must not forward.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeaders forwards end-to-end response headers, skipping hop-by-hop headers
// and Content-Length (which the caller sets explicitly).
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[k] || k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

type chatMeta struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type imagesMeta struct {
	Model string `json:"model"`
	N     int64  `json:"n"`
}

type Gateway struct {
	cfg    *config.Config
	st     *store.Store
	engine *route.Engine
	up     *upstream.Client
	logs   *logWorker
}

func NewGateway(cfg *config.Config, st *store.Store, engine *route.Engine) *Gateway {
	return &Gateway{
		cfg:    cfg,
		st:     st,
		engine: engine,
		up:     upstream.NewClient(120 * time.Second),
		logs:   newLogWorker(context.Background(), st),
	}
}

// ---------- public routing ----------

// Register wires the public OpenAI-compatible endpoints.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("POST /v1/images/generations", g.handleImages)
}

// authenticate validates the caller's Bearer API key and returns its token.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (*domain.Token, bool) {
	key := bearerToken(r)
	if key == "" {
		unauthorized(w, "missing API key. use Authorization: Bearer <your-key>")
		return nil, false
	}
	tok, err := g.st.GetTokenByKey(r.Context(), key)
	if err != nil {
		unauthorized(w, "invalid API key")
		return nil, false
	}
	if !tok.Enabled {
		unauthorized(w, "API key is disabled")
		return nil, false
	}
	u, err := g.st.GetUser(r.Context(), tok.UserID)
	if err != nil || u == nil || !u.Enabled {
		unauthorized(w, "user is disabled")
		return nil, false
	}
	g.st.TouchToken(r.Context(), tok.ID)
	return tok, true
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.authenticate(w, r); !ok {
		return
	}
	vms, err := g.st.ListVirtualModels(r.Context())
	if err != nil {
		openAIError(w, http.StatusInternalServerError, "server_error", "", "failed to list models")
		return
	}
	data := make([]map[string]any, 0, len(vms))
	for _, vm := range vms {
		if !vm.Enabled {
			continue
		}
		data = append(data, map[string]any{"id": vm.Name, "object": "model", "owned_by": "llm-api"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---------- chat completions ----------

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	tok, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "failed to read request body")
		return
	}
	var meta chatMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "invalid JSON body: "+err.Error())
		return
	}
	if meta.Model == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "model is required")
		return
	}
	var bmap map[string]any
	_ = json.Unmarshal(body, &bmap)
	maxTokens := extractMaxTokens(bmap)

	ctx := r.Context()
	excluded := map[string]bool{}
	attempts := 0
	var lastErrStr string
	clientGone := false
	start := time.Now()

	for attempts < maxAttempts {
		attempts++
		tgt, _, err := g.engine.Pick(ctx, meta.Model, maxTokens, 1, int64(g.cfg.DefaultReserveTokens()), excluded)
		if err != nil {
			// Keep the real upstream failure from earlier attempts; the final
			// Pick error only says remaining candidates were already tried.
			if lastErrStr == "" {
				lastErrStr = err.Error()
			}
			break
		}
		err = g.tryChat(ctx, w, tok, meta, body, tgt)
		if err == nil {
			_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), tgt.QuotaID, domain.QuotaStatusOK, "")
			return
		}
		// refund reservation on every failure path
		if rerr := g.st.Release(context.WithoutCancel(ctx), tgt.QuotaID, tgt.Reserved); rerr != nil {
			log.Printf("[gateway] release failed: %v", rerr)
		}
		if ctx.Err() != nil {
			// client disconnected mid-request; not a channel failure — do not
			// mark the candidate unavailable and do not fail over.
			clientGone = true
			lastErrStr = "client disconnected"
			break
		}
		// failover: mark unavailable and try the next candidate
		_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), tgt.QuotaID, domain.QuotaStatusUnavailable, err.Error())
		lastErrStr = err.Error()
		excluded[tgt.Channel.ID.Hex()+"|"+tgt.ActualModel] = true
	}
	if clientGone {
		g.logs.submit(&domain.RequestLog{
			UserID: tok.UserID, TokenID: tok.ID, Endpoint: "/v1/chat/completions",
			VirtualModel: meta.Model, Status: "error", Error: "client disconnected",
			LatencyMs: time.Since(start).Milliseconds(), CreatedAt: time.Now().UTC(),
		})
		log.Printf("[chat] client disconnected %s model=%s", tok.Name, meta.Model)
		return
	}
	if lastErrStr == "" {
		lastErrStr = "all channels unavailable"
	}
	finalErr := fmt.Sprintf("no available channel could serve this request: %s", lastErrStr)
	g.logs.submit(&domain.RequestLog{
		UserID: tok.UserID, TokenID: tok.ID, Endpoint: "/v1/chat/completions",
		VirtualModel: meta.Model, Status: "error", Error: finalErr,
		LatencyMs: time.Since(start).Milliseconds(), CreatedAt: time.Now().UTC(),
	})
	log.Printf("[chat] FAIL %s model=%s err=%s", tok.Name, meta.Model, finalErr)
	openAIError(w, http.StatusServiceUnavailable, "api_error", "no_channel_available", finalErr)
}

// tryChat performs one upstream attempt. On success it writes the response and
// returns nil. A non-nil error means nothing was written to the client yet, so
// the caller may safely fail over to another candidate.
func (g *Gateway) tryChat(ctx context.Context, w http.ResponseWriter, tok *domain.Token, meta chatMeta, body []byte, tgt *route.Target) error {
	start := time.Now()
	reqBody, err := upstream.RewriteModel(body, tgt.ActualModel)
	if err != nil {
		return fmt.Errorf("bad request body: %v", err)
	}
	if meta.Stream {
		reqBody, err = upstream.EnsureStreamUsageOption(reqBody)
		if err != nil {
			return fmt.Errorf("bad request body: %v", err)
		}
	}
	resp, err := g.up.Do(ctx, tgt.Channel, tgt.Key.Key, "/chat/completions", reqBody)
	if err != nil {
		return fmt.Errorf("channel %s: %v", tgt.Channel.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrLen))
		return fmt.Errorf("channel %s: upstream %s: %s", tgt.Channel.Name, resp.Status, strings.TrimSpace(string(eb)))
	}

	if meta.Stream {
		return g.streamChat(ctx, w, resp, tok, tgt, body, meta.Model)
	}

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("channel %s: read response: %v", tgt.Channel.Name, err)
	}
	usage := upstream.ParseChatUsage(out)
	promptEst := upstream.EstimatePromptTokens(body)
	actual, pt, ct, tt := g.settleChat(tgt, usage, promptEst, "")
	// WithoutCancel: settle even if the client disconnected after the upstream
	// response arrived, so the reservation never leaks.
	g.settle(context.WithoutCancel(ctx), tgt, actual, tok, "/v1/chat/completions", meta.Model, tgt.ActualModel, pt, ct, tt, 0, "success", "", time.Since(start).Milliseconds())

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	return nil
}

// streamChat forwards an SSE stream verbatim and settles quota from the final
// usage chunk (or a heuristic estimate).
func (g *Gateway) streamChat(ctx context.Context, w http.ResponseWriter, resp *http.Response, tok *domain.Token, tgt *route.Target, body []byte, virtualModel string) error {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	var usage upstream.Usage
	var completionText strings.Builder
	start := time.Now()

	// Settle exactly once, even if the stream panics or the client disconnects.
	// A non-cancelable context is used so a dropped client cannot leak the
	// reservation (the request context is already canceled at that point).
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[gateway] stream panic: %v", r)
		}
		promptEst := upstream.EstimatePromptTokens(body)
		actual, pt, ct, tt := g.settleChat(tgt, usage, promptEst, completionText.String())
		g.settle(context.WithoutCancel(ctx), tgt, actual, tok, "/v1/chat/completions", virtualModel, tgt.ActualModel, pt, ct, tt, 0, "success", "", time.Since(start).Milliseconds())
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if _, werr := w.Write([]byte(line)); werr != nil {
				// client went away; stop forwarding but still settle
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				if data == "[DONE]" {
					break
				}
				var chunk struct {
					Usage *struct {
						PromptTokens     int64 `json:"prompt_tokens"`
						CompletionTokens int64 `json:"completion_tokens"`
						TotalTokens      int64 `json:"total_tokens"`
					} `json:"usage"`
					Choices []struct {
						Delta struct {
							Content *string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(data), &chunk); err == nil {
					if chunk.Usage != nil {
						usage = upstream.Usage{
							PromptTokens:     chunk.Usage.PromptTokens,
							CompletionTokens: chunk.Usage.CompletionTokens,
							TotalTokens:      chunk.Usage.TotalTokens,
						}
					}
					for _, c := range chunk.Choices {
						if c.Delta.Content != nil {
							completionText.WriteString(*c.Delta.Content)
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return nil
}

// settleChat computes the real quota consumption for a chat attempt.
func (g *Gateway) settleChat(tgt *route.Target, usage upstream.Usage, promptEst int64, completionText string) (actual float64, pt, ct, tt int64) {
	if tgt.Mode == domain.ModeCalls {
		return tgt.Reserved, 0, 0, 0 // one call consumed
	}
	pt, ct, tt = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	if tt == 0 {
		pt = promptEst
		ct = upstream.EstimateCompletionTokens(completionText)
		tt = pt + ct
	}
	mult := tgt.Multiplier
	if mult <= 0 {
		mult = 1
	}
	actual = float64(tt) * mult
	return actual, pt, ct, tt
}

// settle persists consumption: confirm the reservation and write a request log.
func (g *Gateway) settle(ctx context.Context, tgt *route.Target, actual float64, tok *domain.Token, endpoint, virtualModel, actualModel string, pt, ct, tt int64, images int, status, errMsg string, latencyMs int64) {
	// Billing must not be canceled by a client disconnect.
	if err := g.st.Confirm(context.WithoutCancel(ctx), tgt.QuotaID, tgt.Reserved, actual); err != nil {
		log.Printf("[settle] confirm failed for quota %s: %v", tgt.QuotaID.Hex(), err)
	}
	now := time.Now().UTC()
	g.logs.submit(&domain.RequestLog{
		UserID: tok.UserID, TokenID: tok.ID, Endpoint: endpoint,
		VirtualModel:     virtualModel,
		ChannelID:        tgt.Channel.ID,
		ChannelName:      tgt.Channel.Name,
		ActualModel:      actualModel,
		Mode:             tgt.Mode,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
		Images:           images,
		Charge:           actual,
		Status:           status,
		Error:            errMsg,
		LatencyMs:        latencyMs,
		CreatedAt:        now,
	})
}

// ---------- images ----------

func (g *Gateway) handleImages(w http.ResponseWriter, r *http.Request) {
	tok, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "failed to read request body")
		return
	}
	var meta imagesMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "invalid JSON body: "+err.Error())
		return
	}
	if meta.Model == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "model is required")
		return
	}
	if meta.N <= 0 {
		meta.N = 1
	}

	ctx := r.Context()
	excluded := map[string]bool{}
	attempts := 0
	var lastErrStr string
	clientGone := false
	start := time.Now()

	for attempts < maxAttempts {
		attempts++
		tgt, _, err := g.engine.Pick(ctx, meta.Model, 0, meta.N, int64(g.cfg.DefaultReserveTokens()), excluded)
		if err != nil {
			// Keep the real upstream failure from earlier attempts; the final
			// Pick error only says remaining candidates were already tried.
			if lastErrStr == "" {
				lastErrStr = err.Error()
			}
			break
		}
		err = g.tryImages(ctx, w, tok, body, meta, tgt)
		if err == nil {
			_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), tgt.QuotaID, domain.QuotaStatusOK, "")
			return
		}
		// refund reservation on every failure path
		if rerr := g.st.Release(context.WithoutCancel(ctx), tgt.QuotaID, tgt.Reserved); rerr != nil {
			log.Printf("[gateway] release failed: %v", rerr)
		}
		if ctx.Err() != nil {
			// client disconnected mid-request; not a channel failure — do not
			// mark the candidate unavailable and do not fail over.
			clientGone = true
			lastErrStr = "client disconnected"
			break
		}
		// failover: mark unavailable and try the next candidate
		_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), tgt.QuotaID, domain.QuotaStatusUnavailable, err.Error())
		lastErrStr = err.Error()
		excluded[tgt.Channel.ID.Hex()+"|"+tgt.ActualModel] = true
	}
	if clientGone {
		g.logs.submit(&domain.RequestLog{
			UserID: tok.UserID, TokenID: tok.ID, Endpoint: "/v1/images/generations",
			VirtualModel: meta.Model, Status: "error", Error: "client disconnected",
			LatencyMs: time.Since(start).Milliseconds(), CreatedAt: time.Now().UTC(),
		})
		log.Printf("[images] client disconnected %s model=%s", tok.Name, meta.Model)
		return
	}
	if lastErrStr == "" {
		lastErrStr = "all channels unavailable"
	}
	finalErr := fmt.Sprintf("no available channel could serve this request: %s", lastErrStr)
	g.logs.submit(&domain.RequestLog{
		UserID: tok.UserID, TokenID: tok.ID, Endpoint: "/v1/images/generations",
		VirtualModel: meta.Model, Status: "error", Error: finalErr,
		LatencyMs: time.Since(start).Milliseconds(), CreatedAt: time.Now().UTC(),
	})
	openAIError(w, http.StatusServiceUnavailable, "api_error", "no_channel_available", finalErr)
}

func (g *Gateway) tryImages(ctx context.Context, w http.ResponseWriter, tok *domain.Token, body []byte, meta imagesMeta, tgt *route.Target) error {
	start := time.Now()
	reqBody, err := upstream.RewriteModel(body, tgt.ActualModel)
	if err != nil {
		return fmt.Errorf("bad request body: %v", err)
	}
	resp, err := g.up.Do(ctx, tgt.Channel, tgt.Key.Key, "/images/generations", reqBody)
	if err != nil {
		return fmt.Errorf("channel %s: %v", tgt.Channel.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrLen))
		return fmt.Errorf("channel %s: upstream %s: %s", tgt.Channel.Name, resp.Status, strings.TrimSpace(string(eb)))
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("channel %s: read response: %v", tgt.Channel.Name, err)
	}
	images := upstream.CountImages(out)

	// settle: calls-mode charges chargePerCall per actually returned image
	var actual float64
	switch tgt.Mode {
	case domain.ModeCalls:
		cpc := tgt.ChargePerCall
		if cpc <= 0 {
			cpc = 1
		}
		actual = cpc * float64(images)
	default:
		actual = 0 // token mode not meaningful for images; reservation fully refunded
	}
	// WithoutCancel: settle even if the client disconnected, so the
	// reservation never leaks.
	g.settle(context.WithoutCancel(ctx), tgt, actual, tok, "/v1/images/generations", meta.Model, tgt.ActualModel, 0, 0, 0, images, "success", "", time.Since(start).Milliseconds())

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	return nil
}

// ---------- helpers ----------

func extractMaxTokens(bmap map[string]any) int64 {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := bmap[k]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return i
				}
			}
		}
	}
	return 0
}
