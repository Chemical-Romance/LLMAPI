package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is runtime configuration, all controllable via environment variables.
type Config struct {
	MongoURI  string
	MongoDB   string
	HTTPAddr  string
	AdminUser string
	AdminPass string  // empty means auto-generate
	LogQuota  float64 // default reserved tokens per request when max_tokens is unknown
	CoolSec   int     // failover cooldown seconds after an upstream failure
}

func Get() *Config {
	c := &Config{
		MongoURI:  envOr("MONGO_URI", "mongodb://127.0.0.1:27017"),
		MongoDB:   envOr("MONGO_DB", "llm_api"),
		HTTPAddr:  envOr("HTTP_ADDR", ":8080"),
		AdminUser: envOr("ADMIN_USER", "admin"),
		AdminPass: os.Getenv("ADMIN_PASSWORD"),
		LogQuota:  envFloat("DEFAULT_RESERVE_TOKENS", 2048),
		CoolSec:   envInt("FAILOVER_COOLDOWN_SEC", 60),
	}
	return c
}

// DefaultReserveTokens returns the tokens reserved per chat request when the
// client does not specify max_tokens.
func (c *Config) DefaultReserveTokens() int64 {
	return int64(c.LogQuota)
}

// FailoverCooldown returns how long a candidate stays marked unavailable after
// an upstream failure (0 disables the circuit breaker).
func (c *Config) FailoverCooldown() time.Duration {
	return time.Duration(c.CoolSec) * time.Second
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func (c *Config) String() string {
	return fmt.Sprintf("mongo=%s db=%s addr=%s admin=%s", redactURI(c.MongoURI), c.MongoDB, c.HTTPAddr, c.AdminUser)
}

// redactURI hides the password portion of a Mongo connection string so it is
// safe to log.
func redactURI(uri string) string {
	i := strings.Index(uri, "://")
	at := strings.LastIndex(uri, "@")
	if i < 0 || at < 0 {
		return uri
	}
	creds := uri[i+3 : at]
	colon := strings.IndexByte(creds, ':')
	if colon < 0 {
		return uri
	}
	return uri[:i+3] + creds[:colon] + ":***" + uri[at:]
}
