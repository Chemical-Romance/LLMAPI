package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"llm-api/internal/db"
	"llm-api/internal/domain"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrNameConflict = errors.New("name conflict")
	ErrInUse        = errors.New("in use")
)

type Store struct {
	DB *db.DB
}

func New(d *db.DB) *Store { return &Store{DB: d} }

// ---------- helpers ----------

func hashKey(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// RandomKey generates an API key like sk-<64 hex>.
func RandomKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

func now() time.Time { return time.Now().UTC() }

func objID(v any) (bson.ObjectID, error) {
	switch id := v.(type) {
	case bson.ObjectID:
		return id, nil
	case string:
		return bson.ObjectIDFromHex(id)
	default:
		return bson.ObjectID{}, fmt.Errorf("invalid id type %T", v)
	}
}

// ---------- users ----------

func (s *Store) CreateUser(ctx context.Context, name string) (*domain.User, error) {
	u := &domain.User{
		ID:        bson.NewObjectID(),
		Name:      name,
		Enabled:   true,
		CreatedAt: now(),
	}
	if _, err := s.DB.Users.InsertOne(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	cur, err := s.DB.Users.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"createdAt": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.User{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetUser(ctx context.Context, id any) (*domain.User, error) {
	oid, err := objID(id)
	if err != nil {
		return nil, err
	}
	var u domain.User
	if err := s.DB.Users.FindOne(ctx, bson.M{"_id": oid}).Decode(&u); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (s *Store) UpdateUser(ctx context.Context, id any, name string, enabled bool, desc string) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.Users.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{
		"name": name, "enabled": enabled, "description": desc,
	}})
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	// delete tokens first
	_, err = s.DB.Tokens.DeleteMany(ctx, bson.M{"userId": oid})
	if err != nil {
		return err
	}
	_, err = s.DB.Users.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

// ---------- tokens ----------

type NewToken struct {
	UserID bson.ObjectID
	Name   string
}

// CreateToken stores the token hash and returns the plaintext key (shown once).
// Token names must be unique within one user.
func (s *Store) CreateToken(ctx context.Context, n NewToken) (*domain.Token, string, error) {
	exists, err := s.DB.Tokens.CountDocuments(ctx, bson.M{"userId": n.UserID, "name": n.Name})
	if err != nil {
		return nil, "", err
	}
	if exists > 0 {
		return nil, "", ErrNameConflict
	}
	plain := RandomKey()
	t := &domain.Token{
		ID:        bson.NewObjectID(),
		UserID:    n.UserID,
		Name:      n.Name,
		KeyHash:   hashKey(plain),
		Enabled:   true,
		CreatedAt: now(),
	}
	if _, err := s.DB.Tokens.InsertOne(ctx, t); err != nil {
		return nil, "", err
	}
	return t, plain, nil
}

// GetTokenByKey finds a token by plaintext API key.
func (s *Store) GetTokenByKey(ctx context.Context, plain string) (*domain.Token, error) {
	var t domain.Token
	if err := s.DB.Tokens.FindOne(ctx, bson.M{"keyHash": hashKey(plain)}).Decode(&t); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *Store) ListTokens(ctx context.Context) ([]domain.Token, error) {
	cur, err := s.DB.Tokens.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"createdAt": -1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.Token{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) UpdateToken(ctx context.Context, id any, enabled bool, name string) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	set := bson.M{"enabled": enabled}
	if name != "" {
		set["name"] = name
	}
	_, err = s.DB.Tokens.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set})
	return err
}

func (s *Store) DeleteToken(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.Tokens.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

func (s *Store) TouchToken(ctx context.Context, id bson.ObjectID) {
	n := now()
	_, _ = s.DB.Tokens.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"lastUsedAt": &n}})
}

// ---------- channels ----------

func (s *Store) CreateChannel(ctx context.Context, ch *domain.Channel, keys []string) (*domain.Channel, error) {
	ch.ID = bson.NewObjectID()
	ch.CreatedAt = now()
	if ch.TimeoutSec == 0 {
		ch.TimeoutSec = 120
	}
	if _, err := s.DB.Channels.InsertOne(ctx, ch); err != nil {
		return nil, err
	}
	for _, k := range keys {
		k = trimSpace(k)
		if k == "" {
			continue
		}
		_, err := s.DB.ChannelKeys.InsertOne(ctx, &domain.ChannelKey{
			ID:        bson.NewObjectID(),
			ChannelID: ch.ID,
			Key:       k,
			Enabled:   true,
			CreatedAt: now(),
		})
		if err != nil {
			return nil, err
		}
	}
	return ch, nil
}

func (s *Store) ListChannels(ctx context.Context) ([]domain.Channel, error) {
	cur, err := s.DB.Channels.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"createdAt": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.Channel{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetChannel(ctx context.Context, id any) (*domain.Channel, error) {
	oid, err := objID(id)
	if err != nil {
		return nil, err
	}
	var ch domain.Channel
	if err := s.DB.Channels.FindOne(ctx, bson.M{"_id": oid}).Decode(&ch); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &ch, nil
}

func (s *Store) UpdateChannel(ctx context.Context, ch *domain.Channel) error {
	_, err := s.DB.Channels.UpdateOne(ctx, bson.M{"_id": ch.ID}, bson.M{"$set": bson.M{
		"name": ch.Name, "baseUrl": ch.BaseURL, "authType": ch.AuthType,
		"apiKeyHeader": ch.APIKeyHeader, "timeoutSec": ch.TimeoutSec, "enabled": ch.Enabled,
	}})
	return err
}

// DeleteChannel removes the channel and cascades its keys, quotas, and route references.
func (s *Store) DeleteChannel(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	if _, err := s.DB.ChannelKeys.DeleteMany(ctx, bson.M{"channelId": oid}); err != nil {
		return err
	}
	if _, err := s.DB.Quotas.DeleteMany(ctx, bson.M{"channelId": oid}); err != nil {
		return err
	}
	// remove channel from all virtual model candidates
	_, err = s.DB.VirtualModels.UpdateMany(ctx, bson.M{"candidates.channelId": oid},
		bson.M{"$pull": bson.M{"candidates": bson.M{"channelId": oid}}})
	if err != nil {
		return err
	}
	_, err = s.DB.Channels.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

// ---------- channel keys ----------

func (s *Store) ListChannelKeys(ctx context.Context, channelID any) ([]domain.ChannelKey, error) {
	oid, err := objID(channelID)
	if err != nil {
		return nil, err
	}
	cur, err := s.DB.ChannelKeys.Find(ctx, bson.M{"channelId": oid}, options.Find().SetSort(bson.M{"createdAt": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.ChannelKey{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListAllChannelKeys returns every channel key across all channels. Used by the
// admin console to avoid an N+1 query per channel.
func (s *Store) ListAllChannelKeys(ctx context.Context) ([]domain.ChannelKey, error) {
	cur, err := s.DB.ChannelKeys.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"createdAt": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.ChannelKey{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AddChannelKey(ctx context.Context, channelID any, key string) (*domain.ChannelKey, error) {
	oid, err := objID(channelID)
	if err != nil {
		return nil, err
	}
	ck := &domain.ChannelKey{
		ID:        bson.NewObjectID(),
		ChannelID: oid,
		Key:       trimSpace(key),
		Enabled:   true,
		CreatedAt: now(),
	}
	if ck.Key == "" {
		return nil, fmt.Errorf("key is empty")
	}
	if _, err := s.DB.ChannelKeys.InsertOne(ctx, ck); err != nil {
		return nil, err
	}
	return ck, nil
}

func (s *Store) UpdateChannelKeyEnabled(ctx context.Context, id any, enabled bool) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.ChannelKeys.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{"enabled": enabled}})
	return err
}

func (s *Store) DeleteChannelKey(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.ChannelKeys.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

// ---------- quotas ----------

func (s *Store) ListQuotas(ctx context.Context, filter bson.M) ([]domain.Quota, error) {
	cur, err := s.DB.Quotas.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "channelId", Value: 1}, {Key: "model", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.Quota{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetQuota(ctx context.Context, channelID any, model string) (*domain.Quota, error) {
	oid, err := objID(channelID)
	if err != nil {
		return nil, err
	}
	var q domain.Quota
	if err := s.DB.Quotas.FindOne(ctx, bson.M{"channelId": oid, "model": model}).Decode(&q); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &q, nil
}

func (s *Store) CreateQuota(ctx context.Context, q *domain.Quota) (*domain.Quota, error) {
	q.ID = bson.NewObjectID()
	q.CreatedAt = now()
	q.UpdatedAt = q.CreatedAt
	if q.ChargePerCall == 0 {
		q.ChargePerCall = 1
	}
	if q.Multiplier == 0 {
		q.Multiplier = 1
	}
	if _, err := s.DB.Quotas.InsertOne(ctx, q); err != nil {
		var we mongo.WriteException
		if errors.As(err, &we) && len(we.WriteErrors) > 0 && we.WriteErrors[0].Code == 11000 {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	return q, nil
}

func (s *Store) UpdateQuota(ctx context.Context, q *domain.Quota) error {
	q.UpdatedAt = now()
	set := bson.M{
		"channelId": q.ChannelID, "model": q.Model,
		"mode": q.Mode, "type": q.Type, "remaining": q.Remaining, "chargePerCall": q.ChargePerCall,
		"multiplier": q.Multiplier, "enabled": q.Enabled, "note": q.Note, "updatedAt": q.UpdatedAt,
	}
	update := bson.M{"$set": set}
	if q.ExpiresAt != nil {
		set["expiresAt"] = q.ExpiresAt
	} else {
		// "永久" clears any previously configured expiry
		update["$unset"] = bson.M{"expiresAt": ""}
	}
	if _, err := s.DB.Quotas.UpdateOne(ctx, bson.M{"_id": q.ID}, update); err != nil {
		return quotaUpdateErr(err)
	}
	// Editing remaining above zero (and a still-valid expiry) recovers
	// an "insufficient" / "expired" quota.
	if q.Remaining > 0 && !q.IsExpired() {
		_, _ = s.DB.Quotas.UpdateOne(ctx,
			bson.M{"_id": q.ID, "status": bson.M{"$in": bson.A{domain.QuotaStatusInsufficient, domain.QuotaStatusExpired}}},
			bson.M{"$set": bson.M{"status": domain.QuotaStatusOK, "statusReason": ""}})
	}
	return nil
}

// quotaUpdateErr maps a Mongo duplicate-key write error to ErrNameConflict.
func quotaUpdateErr(err error) error {
	var we mongo.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 && we.WriteErrors[0].Code == 11000 {
		return ErrNameConflict
	}
	return err
}

// AdjustQuota adds delta to remaining (positive or negative) and returns the new value.
// SetQuotaStatus records a quota's runtime health status (ok/insufficient/unavailable).
func (s *Store) SetQuotaStatus(ctx context.Context, id any, status, reason string) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	set := bson.M{"status": status, "statusAt": now()}
	if reason == "" {
		set["statusReason"] = ""
	} else {
		set["statusReason"] = reason
	}
	_, err = s.DB.Quotas.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set})
	return err
}

func (s *Store) AdjustQuota(ctx context.Context, id any, delta float64) (float64, error) {
	oid, err := objID(id)
	if err != nil {
		return 0, err
	}
	res := s.DB.Quotas.FindOneAndUpdate(
		ctx,
		bson.M{"_id": oid},
		bson.M{"$inc": bson.M{"remaining": delta}, "$set": bson.M{"updatedAt": now()}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		return 0, err
	}
	var q domain.Quota
	if err := res.Decode(&q); err != nil {
		return 0, err
	}
	// Topping up above zero recovers an "insufficient" quota.
	if q.Remaining > 0 && q.Status == domain.QuotaStatusInsufficient {
		_, _ = s.DB.Quotas.UpdateOne(ctx,
			bson.M{"_id": oid},
			bson.M{"$set": bson.M{"status": domain.QuotaStatusOK, "statusReason": ""}})
	}
	return q.Remaining, nil
}

// ClearReserved releases a leaked in-flight reservation back to remaining and
// resets reserved to 0 (same accounting as a normal Release). It returns the
// amount that was cleared. Useful when a crash/restart leaves a quota stuck in
// "reserved" with no request left to settle it.
func (s *Store) ClearReserved(ctx context.Context, id any) (float64, error) {
	oid, err := objID(id)
	if err != nil {
		return 0, err
	}
	// Pipeline update so remaining += reserved and reserved = 0 happen atomically.
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.M{
			"remaining": bson.M{"$add": bson.A{"$remaining", "$reserved"}},
			"reserved":  float64(0),
			"updatedAt": now(),
		}}},
	}
	res := s.DB.Quotas.FindOneAndUpdate(
		ctx,
		bson.M{"_id": oid},
		pipeline,
		options.FindOneAndUpdate().SetReturnDocument(options.Before),
	)
	if err := res.Err(); err != nil {
		return 0, err
	}
	var before domain.Quota
	if err := res.Decode(&before); err != nil {
		return 0, err
	}
	// Releasing quota above zero recovers an "insufficient" quota.
	if before.Remaining+before.Reserved > 0 && before.Status == domain.QuotaStatusInsufficient {
		_, _ = s.DB.Quotas.UpdateOne(ctx,
			bson.M{"_id": oid},
			bson.M{"$set": bson.M{"status": domain.QuotaStatusOK, "statusReason": ""}})
	}
	return before.Reserved, nil
}

func (s *Store) DeleteQuota(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.Quotas.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

// ---------- quota atomic ops (the heart of the billing engine) ----------

// Reserve atomically takes `need` from remaining into reserved.
// Returns false when quota cannot cover the request (disabled / not enough / no config).
func (s *Store) Reserve(ctx context.Context, quotaID bson.ObjectID, need float64) (bool, error) {
	if need <= 0 {
		need = 1
	}
	res, err := s.DB.Quotas.UpdateOne(ctx,
		bson.M{"_id": quotaID, "enabled": true, "remaining": bson.M{"$gte": need}},
		bson.M{"$inc": bson.M{"remaining": -need, "reserved": need}, "$set": bson.M{"updatedAt": now()}},
	)
	if err != nil {
		return false, err
	}
	return res.ModifiedCount > 0, nil
}

// Confirm settles a reservation: actual is the real consumption.
// If actual < reserved, the difference is returned to remaining.
// If actual > reserved, the extra is charged from remaining.
func (s *Store) Confirm(ctx context.Context, quotaID bson.ObjectID, reserved, actual float64) error {
	if actual < 0 {
		actual = 0
	}
	diff := reserved - actual // +refund / -extra charge
	_, err := s.DB.Quotas.UpdateOne(ctx,
		bson.M{"_id": quotaID},
		bson.M{"$inc": bson.M{"reserved": -reserved, "remaining": diff}, "$set": bson.M{"updatedAt": now()}},
	)
	return err
}

// Release frees a reservation entirely (failure path).
func (s *Store) Release(ctx context.Context, quotaID bson.ObjectID, reserved float64) error {
	_, err := s.DB.Quotas.UpdateOne(ctx,
		bson.M{"_id": quotaID},
		bson.M{"$inc": bson.M{"reserved": -reserved, "remaining": reserved}, "$set": bson.M{"updatedAt": now()}},
	)
	return err
}

// ---------- virtual models ----------

func (s *Store) ListVirtualModels(ctx context.Context) ([]domain.VirtualModel, error) {
	cur, err := s.DB.VirtualModels.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.VirtualModel{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	for i := range out {
		sort.SliceStable(out[i].Candidates, func(a, b int) bool {
			return out[i].Candidates[a].Priority < out[i].Candidates[b].Priority
		})
	}
	return out, nil
}

func (s *Store) GetVirtualModelByName(ctx context.Context, name string) (*domain.VirtualModel, error) {
	var vm domain.VirtualModel
	if err := s.DB.VirtualModels.FindOne(ctx, bson.M{"name": name, "enabled": true}).Decode(&vm); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &vm, nil
}

func (s *Store) GetVirtualModelByID(ctx context.Context, id any) (*domain.VirtualModel, error) {
	oid, err := objID(id)
	if err != nil {
		return nil, err
	}
	var vm domain.VirtualModel
	if err := s.DB.VirtualModels.FindOne(ctx, bson.M{"_id": oid}).Decode(&vm); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &vm, nil
}

func (s *Store) CreateVirtualModel(ctx context.Context, vm *domain.VirtualModel) (*domain.VirtualModel, error) {
	vm.ID = bson.NewObjectID()
	vm.CreatedAt = now()
	vm.UpdatedAt = vm.CreatedAt
	if _, err := s.DB.VirtualModels.InsertOne(ctx, vm); err != nil {
		var we mongo.WriteException
		if errors.As(err, &we) && len(we.WriteErrors) > 0 && we.WriteErrors[0].Code == 11000 {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	return vm, nil
}

func (s *Store) UpdateVirtualModel(ctx context.Context, vm *domain.VirtualModel) error {
	vm.UpdatedAt = now()
	_, err := s.DB.VirtualModels.UpdateOne(ctx, bson.M{"_id": vm.ID}, bson.M{"$set": bson.M{
		"name": vm.Name, "description": vm.Description, "candidates": vm.Candidates,
		"enabled": vm.Enabled, "updatedAt": vm.UpdatedAt,
	}})
	return err
}

// SaveSpeedTestRun persists the latest speed-test results on the virtual model.
func (s *Store) SaveSpeedTestRun(ctx context.Context, id any, results []domain.ProbeResult) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	run := domain.ProbeRun{At: now(), Results: results}
	_, err = s.DB.VirtualModels.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{"speedTest": run}})
	return err
}

// SaveProbeRun persists the latest light-probe results on the virtual model.
func (s *Store) SaveProbeRun(ctx context.Context, id any, results []domain.ProbeResult) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	run := domain.ProbeRun{At: now(), Results: results}
	_, err = s.DB.VirtualModels.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{"probe": run}})
	return err
}

func (s *Store) DeleteVirtualModel(ctx context.Context, id any) error {
	oid, err := objID(id)
	if err != nil {
		return err
	}
	_, err = s.DB.VirtualModels.DeleteOne(ctx, bson.M{"_id": oid})
	return err
}

// ---------- request logs ----------

func (s *Store) InsertLog(ctx context.Context, l *domain.RequestLog) error {
	if l.ID.IsZero() {
		l.ID = bson.NewObjectID()
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = now()
	}
	_, err := s.DB.RequestLogs.InsertOne(ctx, l)
	return err
}

type LogQuery struct {
	Page      int
	Limit     int
	UserID    string
	ChannelID string
	Status    string
	Model     string
	From      time.Time
	To        time.Time
}

func (s *Store) ListLogs(ctx context.Context, q LogQuery) ([]domain.RequestLog, error) {
	f := logFilter(q)
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 20
	}
	skip := int64((q.Page - 1) * q.Limit)
	cur, err := s.DB.RequestLogs.Find(ctx, f, options.Find().
		SetSort(bson.M{"createdAt": -1}).SetSkip(skip).SetLimit(int64(q.Limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []domain.RequestLog{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CountLogs(ctx context.Context, q LogQuery) (int64, error) {
	return s.DB.RequestLogs.CountDocuments(ctx, logFilter(q))
}

// logFilter builds the Mongo filter shared by ListLogs and CountLogs so the
// paginated total always matches the returned page.
func logFilter(q LogQuery) bson.M {
	f := bson.M{}
	if q.UserID != "" {
		if oid, err := bson.ObjectIDFromHex(q.UserID); err == nil {
			f["userId"] = oid
		}
	}
	if q.ChannelID != "" {
		if oid, err := bson.ObjectIDFromHex(q.ChannelID); err == nil {
			f["channelId"] = oid
		}
	}
	if q.Status != "" {
		f["status"] = q.Status
	}
	if q.Model != "" {
		f["virtualModel"] = q.Model
	}
	if !q.From.IsZero() || !q.To.IsZero() {
		rangeF := bson.M{}
		if !q.From.IsZero() {
			rangeF["$gte"] = q.From
		}
		if !q.To.IsZero() {
			rangeF["$lte"] = q.To
		}
		f["createdAt"] = rangeF
	}
	return f
}

// ---------- settings (admin) ----------

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}
