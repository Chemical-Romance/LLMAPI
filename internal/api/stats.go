package api

import (
	"context"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"llm-api/internal/domain"
)

// handleStats returns dashboard aggregates: today + all-time usage, status
// breakdown, top channels/models, quota remaining totals and low quotas.
func (a *Admin) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	type agg struct {
		Requests int64   `bson:"requests" json:"requests"`
		Tokens   int64   `bson:"tokens" json:"tokens"`
		Images   int64   `bson:"images" json:"images"`
		Charge   float64 `bson:"charge" json:"charge"`
		Success  int64   `bson:"success" json:"success"`
		Errors   int64   `bson:"errors" json:"errors"`
	}

	aggregate := func(match bson.M) agg {
		pipeline := []bson.D{
			{{Key: "$match", Value: match}},
			{{Key: "$group", Value: bson.D{
				{Key: "_id", Value: nil},
				{Key: "requests", Value: bson.M{"$sum": 1}},
				{Key: "tokens", Value: bson.M{"$sum": "$totalTokens"}},
				{Key: "images", Value: bson.M{"$sum": "$images"}},
				{Key: "charge", Value: bson.M{"$sum": "$charge"}},
				{Key: "success", Value: bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$status", "success"}}, 1, 0}}}},
				{Key: "errors", Value: bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$ne": bson.A{"$status", "success"}}, 1, 0}}}},
			}}},
		}
		var out []agg
		cur, err := a.st.DB.RequestLogs.Aggregate(ctx, pipeline)
		if err == nil {
			cur.All(ctx, &out)
			cur.Close(ctx)
		}
		if len(out) == 0 {
			return agg{}
		}
		return out[0]
	}

	today := aggregate(bson.M{"createdAt": bson.M{"$gte": startToday}})
	allTime := aggregate(bson.M{})

	statusStats := a.groupStatus(ctx, startToday)
	topChannels := a.groupTop(ctx, "$channelName", 8)
	topModels := a.groupTop(ctx, "$virtualModel", 8)

	quotas, quotaErr := a.st.ListQuotas(ctx, bson.M{})
	if quotaErr != nil {
		quotas = nil
	}
	lowQuotas, remainTokens, remainCalls := quotaSummary(quotas)

	channelCount, _ := a.st.DB.Channels.CountDocuments(ctx, bson.M{})
	vmCount, _ := a.st.DB.VirtualModels.CountDocuments(ctx, bson.M{})
	quotaCount, _ := a.st.DB.Quotas.CountDocuments(ctx, bson.M{})
	userCount, _ := a.st.DB.Users.CountDocuments(ctx, bson.M{})

	writeJSON(w, http.StatusOK, map[string]any{
		"today":             today,
		"allTime":           allTime,
		"statusStats":       statusStats,
		"topChannels":       topChannels,
		"topModels":         topModels,
		"lowQuotas":         lowQuotas,
		"remainTokens":      remainTokens,
		"remainCalls":       remainCalls,
		"channelCount":      channelCount,
		"virtualModelCount": vmCount,
		"quotaCount":        quotaCount,
		"userCount":         userCount,
	})
}

// groupStatus breaks today's logs down by status.
func (a *Admin) groupStatus(ctx context.Context, since time.Time) []map[string]any {
	pipeline := []bson.D{
		{{Key: "$match", Value: bson.M{"createdAt": bson.M{"$gte": since}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "count", Value: bson.M{"$sum": 1}},
			{Key: "charge", Value: bson.M{"$sum": "$charge"}},
			{Key: "tokens", Value: bson.M{"$sum": "$totalTokens"}},
		}}},
		{{Key: "$sort", Value: bson.M{"count": -1}}}}
	var rows []struct {
		Status string  `bson:"_id"`
		Count  int64   `bson:"count"`
		Charge float64 `bson:"charge"`
		Tokens int64   `bson:"tokens"`
	}
	cur, err := a.st.DB.RequestLogs.Aggregate(ctx, pipeline)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	cur.All(ctx, &rows)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"status": row.Status, "count": row.Count, "charge": row.Charge, "tokens": row.Tokens,
		})
	}
	return out
}

// groupTop returns the top-N entries grouped by a field. field must be a full
// aggregation field path such as "$channelName".
// calls is the billed call count for calls-mode logs: 1 per chat call, one per
// generated image for image requests, and 0 for token-mode logs.
// charge is split by mode: chargeTokens for token-mode consumption and
// chargeCalls for calls-mode consumption.
func (a *Admin) groupTop(ctx context.Context, field string, limit int) []map[string]any {
	isCalls := bson.M{"$eq": bson.A{"$mode", domain.ModeCalls}}
	billedCalls := bson.M{"$cond": bson.A{
		isCalls,
		bson.M{"$cond": bson.A{
			bson.M{"$eq": bson.A{"$endpoint", "/v1/images/generations"}},
			bson.M{"$ifNull": bson.A{"$images", 0}},
			1,
		}},
		0,
	}}
	chargeTokens := bson.M{"$cond": bson.A{isCalls, 0, "$charge"}}
	chargeCalls := bson.M{"$cond": bson.A{isCalls, "$charge", 0}}
	pipeline := []bson.D{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: field},
			{Key: "count", Value: bson.M{"$sum": 1}},
			{Key: "calls", Value: bson.M{"$sum": billedCalls}},
			{Key: "tokens", Value: bson.M{"$sum": "$totalTokens"}},
			{Key: "charge", Value: bson.M{"$sum": "$charge"}},
			{Key: "chargeTokens", Value: bson.M{"$sum": chargeTokens}},
			{Key: "chargeCalls", Value: bson.M{"$sum": chargeCalls}},
		}}},
		{{Key: "$sort", Value: bson.M{"count": -1}}},
		{{Key: "$limit", Value: limit}},
	}
	var rows []struct {
		Name         string  `bson:"_id"`
		Count        int64   `bson:"count"`
		Calls        int64   `bson:"calls"`
		Tokens       int64   `bson:"tokens"`
		Charge       float64 `bson:"charge"`
		ChargeTokens float64 `bson:"chargeTokens"`
		ChargeCalls  float64 `bson:"chargeCalls"`
	}
	cur, err := a.st.DB.RequestLogs.Aggregate(ctx, pipeline)
	if err != nil {
		return []map[string]any{}
	}
	defer cur.Close(ctx)
	cur.All(ctx, &rows)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"name": row.Name, "count": row.Count, "calls": row.Calls, "tokens": row.Tokens, "charge": row.Charge,
			"chargeTokens": row.ChargeTokens, "chargeCalls": row.ChargeCalls,
		})
	}
	return out
}

// quotaSummary returns the low-quota warnings plus the totals of remaining
// tokens and remaining calls across all enabled, unexpired quotas.
func quotaSummary(qs []domain.Quota) (low []domain.Quota, remainTokens, remainCalls float64) {
	low = make([]domain.Quota, 0)
	for _, q := range qs {
		if !q.Enabled || q.IsExpired() {
			continue
		}
		if q.Mode == domain.ModeCalls {
			remainCalls += q.Remaining
			if q.Remaining <= 20 {
				low = append(low, q)
			}
			continue
		}
		remainTokens += q.Remaining
		if q.Remaining <= 5000 {
			low = append(low, q)
		}
	}
	return low, remainTokens, remainCalls
}
