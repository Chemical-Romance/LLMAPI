package route

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"llm-api/internal/domain"
	"llm-api/internal/store"
)

// Target is a ready-to-call upstream channel+picked key with a reserved quota.
type Target struct {
	Channel       *domain.Channel
	Key           *domain.ChannelKey
	QuotaID       bson.ObjectID
	Mode          string
	Reserved      float64
	ActualModel   string
	Multiplier    float64
	ChargePerCall float64
}

// AttemptError records why one candidate was skipped/failed, used for diagnostics.
type AttemptError struct {
	ChannelName string
	Model       string
	Reason      string
}

func (e AttemptError) String() string {
	return fmt.Sprintf("%s/%s: %s", e.ChannelName, e.Model, e.Reason)
}

type Engine struct {
	store    *store.Store
	cooldown time.Duration // half-open retry window for "unavailable" quotas
	mu       sync.Mutex
	rr       map[bson.ObjectID]*atomic.Uint64
}

func NewEngine(st *store.Store, cooldown time.Duration) *Engine {
	if cooldown < 0 {
		cooldown = 0
	}
	return &Engine{store: st, cooldown: cooldown, rr: map[bson.ObjectID]*atomic.Uint64{}}
}

func (e *Engine) counter(id bson.ObjectID) *atomic.Uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.rr[id]; ok {
		return c
	}
	c := &atomic.Uint64{}
	e.rr[id] = c
	return c
}

// ReserveNeed computes how much quota must be reserved for a request.
// mode: token -> estimated tokens; calls -> chargePerCall * n.
func ReserveNeed(q *domain.Quota, maxTokens int64, n int64, defaultReserve int64) float64 {
	if q.Mode == domain.ModeCalls {
		if q.ChargePerCall <= 0 {
			q.ChargePerCall = 1
		}
		if n <= 0 {
			n = 1
		}
		return q.ChargePerCall * float64(n)
	}
	// token mode
	mult := q.Multiplier
	if mult <= 0 {
		mult = 1
	}
	if maxTokens <= 0 {
		maxTokens = defaultReserve
	}
	if maxTokens <= 0 {
		maxTokens = 2048 // default reserve when neither client nor config specified
	}
	return math.Ceil(float64(maxTokens) * mult)
}

// Pick resolves a virtual model name into the first usable target.
// It iterates candidates by priority, atomically reserves quota, and returns
// the target plus per-candidate reasons when everything failed. Candidates in
// `exclude` (key "channelID|model") are skipped (already attempted this request),
// and quotas whose status is not "ok" (unavailable/insufficient) are skipped
// so the same broken candidate is not hammered by every request. An
// "unavailable" quota is retried half-open after the cooldown window.
func (e *Engine) Pick(ctx context.Context, virtualModel string, maxTokens, n int64, defaultReserve int64, exclude map[string]bool) (*Target, []AttemptError, error) {
	vm, err := e.store.GetVirtualModelByName(ctx, virtualModel)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, nil, fmt.Errorf("model %q not found or disabled", virtualModel)
		}
		return nil, nil, err
	}
	cands := make([]domain.Candidate, len(vm.Candidates))
	copy(cands, vm.Candidates)
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority < cands[j].Priority })

	var fails []AttemptError
	for _, c := range cands {
		cKey := c.ChannelID.Hex() + "|" + c.Model
		if exclude[cKey] {
			fails = append(fails, AttemptError{ChannelName: "?", Model: c.Model, Reason: "skipped (previous failure)"})
			continue
		}
		if !c.IsEnabled() {
			fails = append(fails, AttemptError{ChannelName: "?", Model: c.Model, Reason: "候选已停用"})
			continue
		}
		tgt, fail := e.tryCandidate(ctx, c, maxTokens, n, defaultReserve)
		if tgt != nil {
			return tgt, nil, nil
		}
		fails = append(fails, *fail)
	}

	// aggregate reasons
	var sb string
	for i, f := range fails {
		if i > 0 {
			sb += "; "
		}
		sb += f.String()
	}
	if sb == "" {
		sb = "no route configured"
	}
	return nil, fails, fmt.Errorf("all candidates failed: %s", sb)
}

// tryCandidate attempts one candidate: loads channel/keys/quota and reserves
// quota. Returns the target, or a diagnostic reason on failure.
func (e *Engine) tryCandidate(ctx context.Context, c domain.Candidate, maxTokens, n int64, defaultReserve int64) (*Target, *AttemptError) {
	ch, err := e.store.GetChannel(ctx, c.ChannelID)
	if err != nil || !ch.Enabled {
		fail := AttemptError{ChannelName: chName(ch), Model: c.Model, Reason: "channel disabled or missing"}
		return nil, &fail
	}
	keys, err := e.store.ListChannelKeys(ctx, ch.ID)
	if err != nil {
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "load keys: " + err.Error()}
		return nil, &fail
	}
	enabledKeys := make([]domain.ChannelKey, 0, len(keys))
	for _, k := range keys {
		if k.Enabled {
			enabledKeys = append(enabledKeys, k)
		}
	}
	if len(enabledKeys) == 0 {
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "no enabled API key"}
		return nil, &fail
	}
	quota, err := e.store.GetQuota(ctx, ch.ID, c.Model)
	if err != nil {
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "quota not configured"}
		return nil, &fail
	}
	if !quota.Enabled {
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "quota disabled"}
		return nil, &fail
	}
	// Validity period: an expired quota is never routed to, and is marked
	// "expired" so the console shows it as 已失效.
	if quota.IsExpired() {
		if quota.Status != domain.QuotaStatusExpired {
			_ = e.store.SetQuotaStatus(ctx, quota.ID, domain.QuotaStatusExpired, "有效期已过")
		}
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model,
			Reason: fmt.Sprintf("有效期已过 (%s)", quota.ExpiresAt.Format("2006-01-02"))}
		return nil, &fail
	}
	// Status gate: only "ok" quotas are routable. An "unavailable" quota is
	// retried half-open once the cooldown window has passed since statusAt.
	if quota.Status == domain.QuotaStatusUnavailable {
		if d := e.cooldown; d > 0 && quota.StatusAt.Add(d).After(time.Now()) {
			left := time.Until(quota.StatusAt.Add(d))
			fail := AttemptError{ChannelName: ch.Name, Model: c.Model,
				Reason: fmt.Sprintf("不可用: %s (%ds后可重试)", reasonSnippet(quota.StatusReason), int(left.Seconds()+0.999))}
			return nil, &fail
		}
		// half-open: fall through and give it one trial
	}
	if quota.Remaining <= 0 {
		if quota.Status != domain.QuotaStatusInsufficient {
			_ = e.store.SetQuotaStatus(ctx, quota.ID, domain.QuotaStatusInsufficient, "insufficient quota")
		}
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model,
			Reason: fmt.Sprintf("insufficient quota (need %.0f, left %.0f)", ReserveNeed(quota, maxTokens, n, defaultReserve), quota.Remaining)}
		return nil, &fail
	}
	if quota.Status == domain.QuotaStatusInsufficient {
		// topped up outside adjust paths — recover
		_ = e.store.SetQuotaStatus(ctx, quota.ID, domain.QuotaStatusOK, "")
		quota.Status = domain.QuotaStatusOK
	}
	if quota.Status == domain.QuotaStatusExpired {
		// expiry extended outside the admin edit path — recover
		_ = e.store.SetQuotaStatus(ctx, quota.ID, domain.QuotaStatusOK, "")
		quota.Status = domain.QuotaStatusOK
	}
	need := ReserveNeed(quota, maxTokens, n, defaultReserve)
	ok, err := e.store.Reserve(ctx, quota.ID, need)
	if err != nil {
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "reserve error: " + err.Error()}
		return nil, &fail
	}
	if !ok {
		_ = e.store.SetQuotaStatus(ctx, quota.ID, domain.QuotaStatusInsufficient, "insufficient quota")
		fail := AttemptError{ChannelName: ch.Name, Model: c.Model,
			Reason: fmt.Sprintf("insufficient quota (need %.0f, left %.0f)", need, quota.Remaining)}
		return nil, &fail
	}
	// pick a key round-robin
	idx := int(e.counter(ch.ID).Add(1)-1) % len(enabledKeys)
	return &Target{
		Channel:       ch,
		Key:           &enabledKeys[idx],
		QuotaID:       quota.ID,
		Mode:          quota.Mode,
		Reserved:      need,
		ActualModel:   c.Model,
		Multiplier:    quota.Multiplier,
		ChargePerCall: quota.ChargePerCall,
	}, nil
}

func chName(ch *domain.Channel) string {
	if ch == nil {
		return "?"
	}
	return ch.Name
}

func reasonSnippet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "upstream failed"
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
