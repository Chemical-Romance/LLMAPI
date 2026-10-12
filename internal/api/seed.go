package api

import (
	"context"
	"crypto/rand"
	"log"

	"llm-api/internal/config"
	"llm-api/internal/db"
)

// HashPassword computes the stored password hash.
func HashPassword(password string) (string, error) { return hashPassword(password) }

// GeneratePassword returns a cryptographically random admin password.
func GeneratePassword() string {
	const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const n = byte(len(chars))
	b := make([]byte, 16)
	buf := make([]byte, 1)
	for i := range b {
		for {
			if _, err := rand.Read(buf); err != nil {
				continue
			}
			if int(buf[0]) < 256-256%int(n) { // reject to avoid modulo bias
				b[i] = chars[int(buf[0])%int(n)]
				break
			}
		}
	}
	return string(b)
}

// EnsureAdmin seeds the admin account on first run. It returns the effective
// admin password when the account was just created (generated or configured),
// and "" when the account already exists.
func EnsureAdmin(ctx context.Context, d *db.DB, cfg *config.Config) (string, error) {
	existing, err := d.GetSetting(ctx, settingAdminUser)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return "", nil
	}
	pass := cfg.AdminPass
	generated := false
	if pass == "" {
		pass = GeneratePassword()
		generated = true
	}
	passHash, err := hashPassword(pass)
	if err != nil {
		return "", err
	}
	if err := d.SetSetting(ctx, settingAdminUser, cfg.AdminUser); err != nil {
		return "", err
	}
	if err := d.SetSetting(ctx, settingAdminPassHash, passHash); err != nil {
		return "", err
	}
	if generated {
		log.Printf("[admin] ⚠ 首次启动，管理员已创建:")
		log.Printf("[admin]   username: %s", cfg.AdminUser)
		log.Printf("[admin]   password: %s  ← 请立即登录并妥善保存", pass)
	}
	return pass, nil
}

// EnsureAuthSecret returns the HMAC secret used to sign admin sessions,
// generating and persisting it on first use.
func EnsureAuthSecret(ctx context.Context, d *db.DB) (string, error) {
	secret, err := d.GetSetting(ctx, settingAuthSecret)
	if err != nil {
		return "", err
	}
	if secret == "" {
		secret = randomHex(24)
		if err := d.SetSetting(ctx, settingAuthSecret, secret); err != nil {
			return "", err
		}
	}
	return secret, nil
}
