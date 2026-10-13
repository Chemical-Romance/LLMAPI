package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"llm-api/internal/domain"
	"llm-api/internal/route"
	"llm-api/internal/upstream"
)

// speedResult is stored per candidate; persisted on the virtual model.
type speedResult = domain.ProbeResult

const (
	speedTestTimeout   = 30 * time.Second
	speedTestMaxTokens = 8
	lightProbeEndpoint = "/models"
)

func sortedCandidates(vm *domain.VirtualModel) []domain.Candidate {
	cands := make([]domain.Candidate, len(vm.Candidates))
	copy(cands, vm.Candidates)
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority < cands[j].Priority })
	return cands
}

// speedPrep loads the channel and first enabled key for one candidate.
// The third return value is a human-readable skip/fail reason ("" on success).
func (g *Gateway) speedPrep(ctx context.Context, c domain.Candidate) (*domain.Channel, string, string) {
	ch, err := g.st.GetChannel(ctx, c.ChannelID)
	if err != nil || !ch.Enabled {
		return nil, "", "渠道不存在或已停用"
	}
	keys, err := g.st.ListChannelKeys(ctx, ch.ID)
	if err != nil {
		return nil, "", "读取密钥失败: " + err.Error()
	}
	for _, k := range keys {
		if k.Enabled {
			return ch, k.Key, ""
		}
	}
	return nil, "", "无可用 API Key"
}

// SpeedTest probes every candidate with a real tiny chat request. Each attempt
// goes through the same reserve -> call -> confirm/release quota flow as real
// traffic, so tokens consumed by the test ARE billed to the channel's quota.
func (g *Gateway) SpeedTest(ctx context.Context, idHex string) ([]speedResult, error) {
	id, err := bson.ObjectIDFromHex(idHex)
	if err != nil {
		return nil, fmt.Errorf("bad id")
	}
	vm, err := g.st.GetVirtualModelByID(ctx, id)
	if err != nil {
		return nil, err
	}
	cands := sortedCandidates(vm)

	payload, _ := json.Marshal(map[string]any{
		"model":      "",
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": speedTestMaxTokens,
		"stream":     false,
	})

	results := make([]speedResult, 0, len(cands))
	for _, c := range cands {
		res := speedResult{ChannelID: c.ChannelID.Hex(), Model: c.Model, Priority: c.Priority}
		ch, key, reason := g.speedPrep(ctx, c)
		if reason != "" {
			res.Error = reason
			results = append(results, res)
			continue
		}
		res.ChannelName = ch.Name

		quota, err := g.st.GetQuota(ctx, ch.ID, c.Model)
		if err != nil || !quota.Enabled {
			res.Error = "额度未配置或已停用"
			results = append(results, res)
			continue
		}
		need := route.ReserveNeed(quota, speedTestMaxTokens, 1, g.cfg.DefaultReserveTokens())
		ok, err := g.st.Reserve(ctx, quota.ID, need)
		if err != nil {
			res.Error = "预留失败: " + err.Error()
			results = append(results, res)
			continue
		}
		if !ok {
			res.Error = fmt.Sprintf("额度不足（需 %.0f，剩 %.0f）", need, quota.Remaining)
			results = append(results, res)
			continue
		}

		body, err := upstream.RewriteModel(payload, c.Model)
		if err != nil {
			_ = g.st.Release(context.WithoutCancel(ctx), quota.ID, need)
			res.Error = "构造请求失败: " + err.Error()
			results = append(results, res)
			continue
		}

		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, speedTestTimeout)
		resp, err := g.up.Do(callCtx, ch, key, "/chat/completions", body)
		if err != nil {
			cancel()
			_ = g.st.Release(context.WithoutCancel(ctx), quota.ID, need)
			res.LatencyMs = time.Since(start).Milliseconds()
			res.Error = err.Error()
			_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), quota.ID, domain.QuotaStatusUnavailable, res.Error)
			results = append(results, res)
			continue
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		res.LatencyMs = time.Since(start).Milliseconds()

		if resp.StatusCode >= 400 {
			_ = g.st.Release(context.WithoutCancel(ctx), quota.ID, need)
			res.Error = fmt.Sprintf("上游 %s: %s", resp.Status, trimErr(string(out)))
			_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), quota.ID, domain.QuotaStatusUnavailable, res.Error)
			results = append(results, res)
			continue
		}

		usage := upstream.ParseChatUsage(out)
		var actual float64
		if quota.Mode == domain.ModeCalls {
			actual = need // one call consumed
		} else {
			mult := quota.Multiplier
			if mult <= 0 {
				mult = 1
			}
			actual = float64(usage.TotalTokens) * mult
		}
		if err := g.st.Confirm(context.WithoutCancel(ctx), quota.ID, need, actual); err != nil {
			log.Printf("[speedtest] confirm failed for quota %s: %v", quota.ID.Hex(), err)
		}
		res.OK = true
		res.Tokens = usage.TotalTokens
		res.Charge = actual
		_ = g.st.SetQuotaStatus(context.WithoutCancel(ctx), quota.ID, domain.QuotaStatusOK, "")
		results = append(results, res)
	}
	if err := g.st.SaveSpeedTestRun(context.WithoutCancel(ctx), vm.ID, results); err != nil {
		log.Printf("[speedtest] persist results failed: %v", err)
	}
	return results, nil
}

// LightProbe hits GET /models on every candidate: measures network/API latency
// and validates the channel key with zero token consumption (no inference).
func (g *Gateway) LightProbe(ctx context.Context, idHex string) ([]speedResult, error) {
	id, err := bson.ObjectIDFromHex(idHex)
	if err != nil {
		return nil, fmt.Errorf("bad id")
	}
	vm, err := g.st.GetVirtualModelByID(ctx, id)
	if err != nil {
		return nil, err
	}
	cands := sortedCandidates(vm)

	results := make([]speedResult, 0, len(cands))
	for _, c := range cands {
		res := speedResult{ChannelID: c.ChannelID.Hex(), Model: c.Model, Priority: c.Priority}
		ch, key, reason := g.speedPrep(ctx, c)
		if reason != "" {
			res.Error = reason
			results = append(results, res)
			continue
		}
		res.ChannelName = ch.Name

		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, speedTestTimeout)
		resp, err := g.up.Get(callCtx, ch, key, lightProbeEndpoint)
		if err != nil {
			cancel()
			res.LatencyMs = time.Since(start).Milliseconds()
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		res.LatencyMs = time.Since(start).Milliseconds()

		if resp.StatusCode >= 400 {
			res.Error = fmt.Sprintf("上游 %s: %s", resp.Status, trimErr(string(out)))
			results = append(results, res)
			continue
		}
		res.OK = true
		results = append(results, res)
	}
	if err := g.st.SaveProbeRun(context.WithoutCancel(ctx), vm.ID, results); err != nil {
		log.Printf("[lightprobe] persist results failed: %v", err)
	}
	return results, nil
}

func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
