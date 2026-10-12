package route

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

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
	store *store.Store
	mu    sync.Mutex
	rr    map[bson.ObjectID]*atomic.Uint64
}

func NewEngine(st *store.Store) *Engine {
	return &Engine{store: st, rr: map[bson.ObjectID]*atomic.Uint64{}}
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
// `exclude` (key "channelID|model") are skipped (already attempted channels).
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
		ch, err := e.store.GetChannel(ctx, c.ChannelID)
		if err != nil || !ch.Enabled {
			fails = append(fails, AttemptError{ChannelName: chName(ch), Model: c.Model, Reason: "channel disabled or missing"})
			continue
		}
		keys, err := e.store.ListChannelKeys(ctx, ch.ID)
		if err != nil {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "load keys: " + err.Error()})
			continue
		}
		enabledKeys := make([]domain.ChannelKey, 0, len(keys))
		for _, k := range keys {
			if k.Enabled {
				enabledKeys = append(enabledKeys, k)
			}
		}
		if len(enabledKeys) == 0 {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "no enabled API key"})
			continue
		}
		quota, err := e.store.GetQuota(ctx, ch.ID, c.Model)
		if err != nil {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "quota not configured"})
			continue
		}
		if !quota.Enabled {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "quota disabled"})
			continue
		}
		need := ReserveNeed(quota, maxTokens, n, defaultReserve)
		ok, err := e.store.Reserve(ctx, quota.ID, need)
		if err != nil {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: "reserve error: " + err.Error()})
			continue
		}
		if !ok {
			fails = append(fails, AttemptError{ChannelName: ch.Name, Model: c.Model, Reason: fmt.Sprintf("insufficient quota (need %.0f, left %.0f)", need, quota.Remaining)})
			continue
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
		}, nil, nil
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

func chName(ch *domain.Channel) string {
	if ch == nil {
		return "?"
	}
	return ch.Name
}
