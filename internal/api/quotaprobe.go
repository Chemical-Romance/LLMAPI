package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"llm-api/internal/domain"
	"llm-api/internal/route"
	"llm-api/internal/upstream"
)

// quotaTestMaxTokens bounds the real request used by a quota test.
const quotaTestMaxTokens = 8

// QuotaTestStep is one diagnostic step of a single-quota availability test.
// Steps let the console show exactly where a model became unusable.
type QuotaTestStep struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// QuotaTestResult is the outcome of testing one channel+model quota. A failed
// test still returns HTTP 200 with OK=false and a human-readable Error, so the
// console can render the full diagnostic instead of a bare "HTTP 400".
type QuotaTestResult struct {
	OK          bool            `json:"ok"`
	ChannelID   string          `json:"channelId"`
	ChannelName string          `json:"channelName,omitempty"`
	Model       string          `json:"model"`
	Type        string          `json:"type"`
	Mode        string          `json:"mode"`
	Endpoint    string          `json:"endpoint,omitempty"`
	LatencyMs   int64           `json:"latencyMs"`
	Tokens      int64           `json:"tokens,omitempty"`
	Charge      float64         `json:"charge,omitempty"`
	Error       string          `json:"error,omitempty"`
	Steps       []QuotaTestStep `json:"steps"`
}

// quotaTestRequest builds the upstream endpoint and payload for a quota's model
// type. It mirrors the public gateway endpoints so a test exercises the same
// path a real call would take.
func quotaTestRequest(modelType, model string) (string, []byte) {
	switch modelType {
	case domain.ModelTypeImage:
		b, _ := json.Marshal(map[string]any{"model": model, "prompt": "a small red dot", "n": 1})
		return "/images/generations", b
	case domain.ModelTypeEmbed:
		b, _ := json.Marshal(map[string]any{"model": model, "input": "ping"})
		return "/embeddings", b
	case domain.ModelTypeSpeech:
		b, _ := json.Marshal(map[string]any{"model": model, "input": "ping", "voice": "alloy"})
		return "/audio/speech", b
	default: // text | video | other
		b, _ := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": quotaTestMaxTokens,
			"stream":     false,
		})
		return "/chat/completions", b
	}
}

// TestQuota checks whether a single channel+model quota can currently serve a
// request. It walks the same steps as live routing (channel -> key -> quota
// state -> reserve -> upstream) and records a reason at the first failure, so
// operators can pinpoint why a model is unusable. A tiny real request is sent
// and billed against the quota, exactly like the routing speed test. The test
// is diagnostic only: it does not change the quota's health status.
func (g *Gateway) TestQuota(ctx context.Context, idHex string) (*QuotaTestResult, error) {
	id, err := bson.ObjectIDFromHex(idHex)
	if err != nil {
		return nil, fmt.Errorf("bad id")
	}
	q, err := g.st.GetQuotaByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("quota not found")
	}

	res := &QuotaTestResult{
		ChannelID: q.ChannelID.Hex(),
		Model:     q.Model,
		Type:      normalizeModelType(q.Type),
		Mode:      q.Mode,
	}
	// fail records the failing step and returns the result with HTTP 200 so the
	// caller renders the diagnostic rather than a generic error.
	fail := func(step, msg string) (*QuotaTestResult, error) {
		res.OK = false
		res.Error = msg
		res.Steps = append(res.Steps, QuotaTestStep{Name: step, OK: false, Message: msg})
		return res, nil
	}

	// 1) channel
	ch, err := g.st.GetChannel(ctx, q.ChannelID)
	if err != nil || !ch.Enabled {
		return fail("渠道", "渠道不存在或已停用")
	}
	res.ChannelName = ch.Name
	res.Steps = append(res.Steps, QuotaTestStep{Name: "渠道", OK: true, Message: ch.Name})

	// 2) API key
	keys, err := g.st.ListChannelKeys(ctx, ch.ID)
	if err != nil {
		return fail("密钥", "读取密钥失败: "+err.Error())
	}
	key := ""
	for _, k := range keys {
		if k.Enabled {
			key = k.Key
			break
		}
	}
	if key == "" {
		return fail("密钥", "无可用 API Key（未添加或全部停用）")
	}
	res.Steps = append(res.Steps, QuotaTestStep{Name: "密钥", OK: true, Message: "已选用一个启用中的密钥"})

	// 3) quota state
	if !q.Enabled {
		return fail("额度", "额度已停用")
	}
	if q.IsExpired() {
		return fail("额度", "额度已过期")
	}
	if q.Status == domain.QuotaStatusUnavailable && q.StatusReason != "" {
		res.Steps = append(res.Steps, QuotaTestStep{Name: "状态", OK: true, Message: "当前标记不可用，本次强制重试：" + q.StatusReason})
	}

	// 4) build request + reserve quota
	endpoint, payload := quotaTestRequest(res.Type, q.Model)
	res.Endpoint = endpoint
	need := route.ReserveNeed(q, quotaTestMaxTokens, 1, g.cfg.DefaultReserveTokens())
	ok, err := g.st.Reserve(ctx, q.ID, need)
	if err != nil {
		return fail("额度", "预留失败: "+err.Error())
	}
	if !ok {
		return fail("额度", fmt.Sprintf("额度不足（本次需 %.0f，剩余 %.0f）", need, q.Remaining))
	}
	res.Steps = append(res.Steps, QuotaTestStep{Name: "额度", OK: true, Message: fmt.Sprintf("预留 %.0f", need)})

	// 5) upstream call
	start := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, speedTestTimeout)
	resp, err := g.up.Do(callCtx, ch, key, endpoint, payload)
	if err != nil {
		cancel()
		_ = g.st.Release(context.WithoutCancel(ctx), q.ID, need)
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = "请求失败: " + err.Error()
		res.Steps = append(res.Steps, QuotaTestStep{Name: "上游调用", OK: false, Message: res.Error})
		return res, nil
	}
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body.Close()
	cancel()
	res.LatencyMs = time.Since(start).Milliseconds()

	if resp.StatusCode >= 400 {
		_ = g.st.Release(context.WithoutCancel(ctx), q.ID, need)
		res.Error = fmt.Sprintf("上游 %s: %s", resp.Status, trimErr(string(out)))
		res.Steps = append(res.Steps, QuotaTestStep{Name: "上游调用", OK: false, Message: res.Error})
		return res, nil
	}

	// 6) settle (bill the tiny request, mirroring the speed test)
	usage := upstream.ParseChatUsage(out)
	var actual float64
	if q.Mode == domain.ModeCalls {
		actual = need // one call consumed
	} else {
		mult := q.Multiplier
		if mult <= 0 {
			mult = 1
		}
		actual = float64(usage.TotalTokens) * mult
	}
	if err := g.st.Confirm(context.WithoutCancel(ctx), q.ID, need, actual); err != nil {
		log.Printf("[quotatest] confirm failed for quota %s: %v", q.ID.Hex(), err)
	}
	res.OK = true
	res.Tokens = usage.TotalTokens
	res.Charge = actual
	res.Steps = append(res.Steps, QuotaTestStep{Name: "上游调用", OK: true, Message: fmt.Sprintf("HTTP %s", resp.Status)})
	return res, nil
}
