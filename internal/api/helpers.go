package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const adminSessionTTL = 12 * time.Hour

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// openAIError writes an OpenAI-compatible error body.
func openAIError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
}

func unauthorized(w http.ResponseWriter, msg string) {
	openAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", msg)
}

// ---- admin session tokens (HMAC-signed, stateless) ----

func signPayload(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newAdminToken(secret string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(adminSessionTTL).Unix())))
	return payload + "." + signPayload(secret, payload)
}

func verifyAdminToken(secret, token string) (bool, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false, fmt.Errorf("bad token format")
	}
	payload, sig := parts[0], parts[1]
	if !hmac.Equal([]byte(sig), []byte(signPayload(secret, payload))) {
		return false, fmt.Errorf("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false, err
	}
	var p struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, err
	}
	if p.Exp < time.Now().Unix() {
		return false, fmt.Errorf("expired")
	}
	return true, nil
}

// requestBaseURL reconstructs the externally visible origin (scheme://host)
// from a request, honouring the reverse proxy's X-Forwarded-Proto header. It is
// used to show the public Base URL on the console without hard-coding a domain.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// requireAdmin is middleware: verifies the admin session token.
func requireAdmin(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing admin token")
			return
		}
		ok, err := verifyAdminToken(secret, tok)
		if err != nil || !ok {
			writeErr(w, http.StatusUnauthorized, "invalid or expired admin token")
			return
		}
		next(w, r)
	}
}
