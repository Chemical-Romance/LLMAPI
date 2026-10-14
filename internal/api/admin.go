package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/crypto/bcrypt"

	"llm-api/internal/domain"
	"llm-api/internal/store"
)

const (
	settingAdminUser     = "adminUser"
	settingAdminPassHash = "adminPassHash"
	settingAdminSalt     = "adminPassSalt"
	settingAuthSecret    = "authSecret"
)

// ---------- password helpers ----------

// hashPassword derives a bcrypt hash for the given password.
func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// legacyPasswordHash reproduces the old single-round SHA-256 scheme so existing
// admin accounts keep working until they are upgraded on the next login.
func legacyPasswordHash(salt, password string) string {
	h := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(h[:])
}

// isLegacyHash reports whether s is a 64-char hex SHA-256 digest.
func isLegacyHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// maskSecret keeps a short prefix/suffix so a key can be identified without
// exposing the full value.
func maskSecret(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// maskedKey renders one channel key with the secret redacted.
func maskedKey(k domain.ChannelKey) map[string]any {
	return map[string]any{
		"id": k.ID.Hex(), "channelId": k.ChannelID.Hex(),
		"key": maskSecret(k.Key), "enabled": k.Enabled, "createdAt": k.CreatedAt,
	}
}

// maskedKeys renders channel keys with the secret redacted for admin display.
func maskedKeys(keys []domain.ChannelKey) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, maskedKey(k))
	}
	return out
}

// nonNilQuotas ensures JSON renders [] instead of null when a channel has no
// quota configured.
func nonNilQuotas(qs []domain.Quota) []domain.Quota {
	if qs == nil {
		return []domain.Quota{}
	}
	return qs
}

const (
	loginWindow = 5 * time.Minute
	loginMax    = 10
)

// loginLimiter throttles admin login attempts per client IP.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string][]time.Time{}}
}

// allow records an attempt and reports whether it is permitted.
func (l *loginLimiter) allow(key string) bool {
	now := time.Now()
	cutoff := now.Add(-loginWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= loginMax {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)
	return true
}

// clientIP extracts the caller IP, honouring a single X-Forwarded-For hop.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// AdminAuth bundles the settings needed for admin sessions.
type AdminAuth struct {
	secret string
}

func NewAdminAuth(secret string) *AdminAuth { return &AdminAuth{secret: secret} }

// Admin holds shared dependencies for the admin endpoints.
type Admin struct {
	st      *store.Store
	auth    *AdminAuth
	limiter *loginLimiter
	gw      *Gateway
}

func NewAdmin(st *store.Store, auth *AdminAuth, gw *Gateway) *Admin {
	return &Admin{st: st, auth: auth, limiter: newLoginLimiter(), gw: gw}
}

// Register wires the admin API under /api/admin.
func (a *Admin) Register(mux *http.ServeMux) {
	// public
	mux.HandleFunc("POST /api/admin/login", a.handleLogin)

	// protected
	protect := func(h http.HandlerFunc) http.HandlerFunc { return requireAdmin(a.auth.secret, h) }

	mux.HandleFunc("GET /api/admin/me", protect(a.handleMe))

	mux.HandleFunc("GET /api/admin/channels", protect(a.handleListChannels))
	mux.HandleFunc("POST /api/admin/channels", protect(a.handleCreateChannel))
	mux.HandleFunc("GET /api/admin/channels/{id}", protect(a.handleGetChannel))
	mux.HandleFunc("PUT /api/admin/channels/{id}", protect(a.handleUpdateChannel))
	mux.HandleFunc("DELETE /api/admin/channels/{id}", protect(a.handleDeleteChannel))
	mux.HandleFunc("POST /api/admin/channels/{id}/keys", protect(a.handleAddChannelKey))
	mux.HandleFunc("PUT /api/admin/keys/{id}", protect(a.handleUpdateChannelKey))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", protect(a.handleDeleteChannelKey))

	mux.HandleFunc("GET /api/admin/quotas", protect(a.handleListQuotas))
	mux.HandleFunc("POST /api/admin/quotas", protect(a.handleCreateQuota))
	mux.HandleFunc("PUT /api/admin/quotas/{id}", protect(a.handleUpdateQuota))
	mux.HandleFunc("DELETE /api/admin/quotas/{id}", protect(a.handleDeleteQuota))
	mux.HandleFunc("POST /api/admin/quotas/{id}/adjust", protect(a.handleAdjustQuota))
	mux.HandleFunc("POST /api/admin/quotas/{id}/reset-status", protect(a.handleResetQuotaStatus))

	mux.HandleFunc("GET /api/admin/virtual-models", protect(a.handleListVirtualModels))
	mux.HandleFunc("POST /api/admin/virtual-models", protect(a.handleCreateVirtualModel))
	mux.HandleFunc("PUT /api/admin/virtual-models/{id}", protect(a.handleUpdateVirtualModel))
	mux.HandleFunc("DELETE /api/admin/virtual-models/{id}", protect(a.handleDeleteVirtualModel))
	mux.HandleFunc("POST /api/admin/virtual-models/{id}/speedtest", protect(a.handleSpeedTest))
	mux.HandleFunc("POST /api/admin/virtual-models/{id}/probe", protect(a.handleLightProbe))

	mux.HandleFunc("GET /api/admin/users", protect(a.handleListUsers))
	mux.HandleFunc("POST /api/admin/users", protect(a.handleCreateUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", protect(a.handleUpdateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", protect(a.handleDeleteUser))
	mux.HandleFunc("POST /api/admin/users/{id}/tokens", protect(a.handleCreateUserToken))
	mux.HandleFunc("GET /api/admin/tokens", protect(a.handleListTokens))
	mux.HandleFunc("PUT /api/admin/tokens/{id}", protect(a.handleUpdateToken))
	mux.HandleFunc("DELETE /api/admin/tokens/{id}", protect(a.handleDeleteToken))

	mux.HandleFunc("GET /api/admin/logs", protect(a.handleLogs))
	mux.HandleFunc("GET /api/admin/stats", protect(a.handleStats))
}

// ---------- auth ----------

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	ctx := r.Context()
	user, _ := a.st.DB.GetSetting(ctx, settingAdminUser)
	passHash, _ := a.st.DB.GetSetting(ctx, settingAdminPassHash)
	salt, _ := a.st.DB.GetSetting(ctx, settingAdminSalt)
	if user == "" {
		writeErr(w, http.StatusInternalServerError, "admin account not initialized")
		return
	}
	if salt == "" {
		salt = user // legacy installs hashed with the username as salt
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(user)) == 1
	passOK := false
	if passHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(passHash), []byte(req.Password)) == nil {
			passOK = true
		} else if isLegacyHash(passHash) {
			passOK = subtle.ConstantTimeCompare([]byte(legacyPasswordHash(salt, req.Password)), []byte(passHash)) == 1
			if passOK {
				// transparently upgrade the stored hash to bcrypt
				if h, err := hashPassword(req.Password); err == nil {
					_ = a.st.DB.SetSetting(ctx, settingAdminPassHash, h)
				}
			}
		}
	}
	if !userOK || !passOK {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token := newAdminToken(a.auth.secret)
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expiresIn": int64(adminSessionTTL.Seconds()), "username": user,
	})
}

func (a *Admin) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"username": "admin", "ok": true})
}

// ---------- channels ----------

type channelReq struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"baseUrl"`
	AuthType     string   `json:"authType"`
	APIKeyHeader string   `json:"apiKeyHeader"`
	TimeoutSec   int      `json:"timeoutSec"`
	Enabled      *bool    `json:"enabled"`
	Keys         []string `json:"keys"`
}

func channelFromReq(req channelReq, id bson.ObjectID, createdAt time.Time) *domain.Channel {
	ch := &domain.Channel{
		ID:           id,
		Name:         strings.TrimSpace(req.Name),
		BaseURL:      strings.TrimSpace(req.BaseURL),
		AuthType:     req.AuthType,
		APIKeyHeader: strings.TrimSpace(req.APIKeyHeader),
		TimeoutSec:   req.TimeoutSec,
		CreatedAt:    createdAt,
	}
	if ch.AuthType == "" {
		ch.AuthType = domain.AuthTypeBearer
	}
	if ch.TimeoutSec <= 0 {
		ch.TimeoutSec = 300
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	} else {
		ch.Enabled = true
	}
	return ch
}

func (a *Admin) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.BaseURL) == "" {
		writeErr(w, http.StatusBadRequest, "name and baseUrl are required")
		return
	}
	ch := channelFromReq(req, bson.NewObjectID(), time.Now().UTC())
	created, err := a.st.CreateChannel(r.Context(), ch, req.Keys)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *Admin) handleListChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chs, err := a.st.ListChannels(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Load keys/quotas once and group them, avoiding an N+1 query per channel.
	keys, _ := a.st.ListAllChannelKeys(ctx)
	quotas, _ := a.st.ListQuotas(ctx, bson.M{})
	keysByCh := make(map[bson.ObjectID][]domain.ChannelKey)
	for _, k := range keys {
		keysByCh[k.ChannelID] = append(keysByCh[k.ChannelID], k)
	}
	quotasByCh := make(map[bson.ObjectID][]domain.Quota)
	for _, q := range quotas {
		quotasByCh[q.ChannelID] = append(quotasByCh[q.ChannelID], q)
	}
	out := make([]map[string]any, 0, len(chs))
	for _, ch := range chs {
		out = append(out, map[string]any{
			"id": ch.ID.Hex(), "name": ch.Name, "baseUrl": ch.BaseURL,
			"authType": ch.AuthType, "apiKeyHeader": ch.APIKeyHeader,
			"timeoutSec": ch.TimeoutSec, "enabled": ch.Enabled, "createdAt": ch.CreatedAt,
			"keys": maskedKeys(keysByCh[ch.ID]), "quotas": nonNilQuotas(quotasByCh[ch.ID]),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) handleGetChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ch, err := a.st.GetChannel(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	keys, _ := a.st.ListChannelKeys(ctx, ch.ID)
	quotas, _ := a.st.ListQuotas(ctx, bson.M{"channelId": ch.ID})
	writeJSON(w, http.StatusOK, map[string]any{
		"id": ch.ID.Hex(), "name": ch.Name, "baseUrl": ch.BaseURL,
		"authType": ch.AuthType, "apiKeyHeader": ch.APIKeyHeader,
		"timeoutSec": ch.TimeoutSec, "enabled": ch.Enabled, "createdAt": ch.CreatedAt,
		"keys": maskedKeys(keys), "quotas": nonNilQuotas(quotas),
	})
}

func (a *Admin) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	existing, err := a.st.GetChannel(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	var req channelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	ch := channelFromReq(req, id, existing.CreatedAt)
	if err := a.st.UpdateChannel(r.Context(), ch); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (a *Admin) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteChannel(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleAddChannelKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	ck, err := a.st.AddChannelKey(r.Context(), r.PathValue("id"), req.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, maskedKey(*ck))
}

func (a *Admin) handleUpdateChannelKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.st.UpdateChannelKeyEnabled(r.Context(), r.PathValue("id"), req.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleDeleteChannelKey(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteChannelKey(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- quotas ----------

// normalizeModelType maps unknown/empty type values to a known set.
func normalizeModelType(t string) string {
	switch t {
	case domain.ModelTypeSpeech, domain.ModelTypeImage, domain.ModelTypeVideo,
		domain.ModelTypeEmbed, domain.ModelTypeOther:
		return t
	default:
		return domain.ModelTypeText
	}
}

func (a *Admin) handleListQuotas(w http.ResponseWriter, r *http.Request) {
	filter := bson.M{}
	if cid := r.URL.Query().Get("channelId"); cid != "" {
		if oid, err := bson.ObjectIDFromHex(cid); err == nil {
			filter["channelId"] = oid
		}
	}
	qs, err := a.st.ListQuotas(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, qs)
}

func (a *Admin) handleCreateQuota(w http.ResponseWriter, r *http.Request) {
	var q domain.Quota
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if q.ChannelID.IsZero() || q.Model == "" {
		writeErr(w, http.StatusBadRequest, "channelId and model are required")
		return
	}
	if q.Mode != domain.ModeCalls {
		q.Mode = domain.ModeToken
	}
	q.Type = normalizeModelType(q.Type)
	created, err := a.st.CreateQuota(r.Context(), &q)
	if err != nil {
		if err == store.ErrNameConflict {
			writeErr(w, http.StatusConflict, "quota already exists for this channel+model")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *Admin) handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	id, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var q domain.Quota
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	q.ID = id
	if q.ChannelID.IsZero() || q.Model == "" {
		writeErr(w, http.StatusBadRequest, "channelId and model are required")
		return
	}
	if q.Mode != domain.ModeCalls {
		q.Mode = domain.ModeToken
	}
	q.Type = normalizeModelType(q.Type)
	if err := a.st.UpdateQuota(r.Context(), &q); err != nil {
		if err == store.ErrNameConflict {
			writeErr(w, http.StatusConflict, "quota already exists for this channel+model")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (a *Admin) handleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteQuota(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleAdjustQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Delta float64 `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	newRemaining, err := a.st.AdjustQuota(r.Context(), r.PathValue("id"), req.Delta)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remaining": newRemaining})
}

// handleResetQuotaStatus manually recovers a quota to "ok" status.
func (a *Admin) handleResetQuotaStatus(w http.ResponseWriter, r *http.Request) {
	if err := a.st.SetQuotaStatus(r.Context(), r.PathValue("id"), domain.QuotaStatusOK, ""); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- virtual models ----------

func (a *Admin) handleListVirtualModels(w http.ResponseWriter, r *http.Request) {
	vms, err := a.st.ListVirtualModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

func (a *Admin) handleCreateVirtualModel(w http.ResponseWriter, r *http.Request) {
	var vm domain.VirtualModel
	if err := json.NewDecoder(r.Body).Decode(&vm); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if vm.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	created, err := a.st.CreateVirtualModel(r.Context(), &vm)
	if err != nil {
		if err == store.ErrNameConflict {
			writeErr(w, http.StatusConflict, "virtual model name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *Admin) handleUpdateVirtualModel(w http.ResponseWriter, r *http.Request) {
	id, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var vm domain.VirtualModel
	if err := json.NewDecoder(r.Body).Decode(&vm); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	vm.ID = id
	if err := a.st.UpdateVirtualModel(r.Context(), &vm); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (a *Admin) handleDeleteVirtualModel(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteVirtualModel(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSpeedTest probes each candidate with a real tiny request (quota billed).
func (a *Admin) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	if a.gw == nil {
		writeErr(w, http.StatusServiceUnavailable, "gateway not ready")
		return
	}
	results, err := a.gw.SpeedTest(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleLightProbe hits GET /models on each candidate (zero token cost).
func (a *Admin) handleLightProbe(w http.ResponseWriter, r *http.Request) {
	if a.gw == nil {
		writeErr(w, http.StatusServiceUnavailable, "gateway not ready")
		return
	}
	results, err := a.gw.LightProbe(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---------- users & tokens ----------

func (a *Admin) handleListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := a.st.ListUsers(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Count tokens per user in one pass instead of loading all tokens per user.
	tokens, _ := a.st.ListTokens(ctx)
	countByUser := make(map[bson.ObjectID]int, len(users))
	for _, t := range tokens {
		countByUser[t.UserID]++
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id": u.ID.Hex(), "name": u.Name, "enabled": u.Enabled,
			"description": u.Description, "createdAt": u.CreatedAt, "tokenCount": countByUser[u.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	u, err := a.st.CreateUser(r.Context(), req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *Admin) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.st.UpdateUser(r.Context(), r.PathValue("id"), req.Name, req.Enabled, req.Description); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleCreateUserToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	oid, err := bson.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad user id")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	tok, plain, err := a.st.CreateToken(r.Context(), store.NewToken{UserID: oid, Name: req.Name})
	if err != nil {
		if errors.Is(err, store.ErrNameConflict) {
			writeErr(w, http.StatusConflict, "该用户下已存在同名令牌")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": tok.ID.Hex(), "name": tok.Name, "userId": tok.UserID.Hex(),
		"enabled": tok.Enabled, "createdAt": tok.CreatedAt,
		"key": plain, // shown only once
	})
}

func (a *Admin) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := a.st.ListTokens(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, t := range toks {
		out = append(out, map[string]any{
			"id": t.ID.Hex(), "userId": t.UserID.Hex(), "name": t.Name,
			"enabled": t.Enabled, "lastUsedAt": t.LastUsedAt, "createdAt": t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.st.UpdateToken(r.Context(), r.PathValue("id"), req.Enabled, req.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteToken(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- misc ----------

func (a *Admin) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := store.LogQuery{
		Page:      parseInt(q.Get("page"), 1),
		Limit:     parseInt(q.Get("limit"), 20),
		UserID:    q.Get("userId"),
		ChannelID: q.Get("channelId"),
		Status:    q.Get("status"),
		Model:     q.Get("model"),
	}
	logs, err := a.st.ListLogs(r.Context(), lq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := a.st.CountLogs(r.Context(), lq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": logs, "total": total, "page": lq.Page, "limit": lq.Limit})
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
