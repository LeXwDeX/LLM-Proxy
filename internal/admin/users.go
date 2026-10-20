package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// DefaultAdminUsername 是启动 seed 流程写入的引导账号名（cmd/proxy/main.go）。
// 该账号禁止经用户管理接口修改/删除：删除或禁用会在下次启动触发 seed 重置，
// 破坏运维连续性（AGENTS.md 红线）；自助改密走 /api/change-password 不受影响。
const DefaultAdminUsername = "admin"

// AuthenticateUser verifies credentials against the nosql UserStore.
func AuthenticateUser(store *nosql.UserStore, username, password string) (config.AdminUser, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return config.AdminUser{}, errors.New("invalid credentials")
	}
	user, err := store.Get(username)
	if err != nil {
		return config.AdminUser{}, errors.New("invalid credentials")
	}
	if user.Disabled {
		return config.AdminUser{}, errors.New("account is disabled")
	}
	ok, err := verifyPasswordHash(user.PasswordHash, password)
	if err != nil {
		return config.AdminUser{}, err
	}
	if !ok {
		return config.AdminUser{}, errors.New("invalid credentials")
	}
	if strings.TrimSpace(user.Role) == "" {
		user.Role = RoleAdmin
	}
	return user, nil
}

// ApplyFixedAdminPassword force-resets the given user's password, discarding
// the old hash. 用于测试实例的 admin_password_fix 机制（议题 #69）：每次启动
// 都把密码恢复为配置的固定值，保证重启/重建后约定凭据始终可用。
func ApplyFixedAdminPassword(store *nosql.UserStore, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username must not be empty")
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("password must not be empty")
	}
	user, err := store.Get(username)
	if err != nil {
		return fmt.Errorf("load user %q: %w", username, err)
	}
	hash, err := HashPasswordWithRandomSalt(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user.PasswordHash = hash
	return store.Update(username, user)
}

// HashPasswordWithRandomSalt generates a random 16-byte salt and hashes the password.
func HashPasswordWithRandomSalt(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	return HashPassword(password, hex.EncodeToString(salt)), nil
}

// HashPassword returns a deterministic SHA-256 based password hash string.
func HashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(salt) + ":" + password))
	return fmt.Sprintf("sha256$%s$%s", strings.TrimSpace(salt), hex.EncodeToString(sum[:]))
}

func verifyPasswordHash(encoded, password string) (bool, error) {
	encoded = strings.TrimSpace(encoded)
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 {
		return false, fmt.Errorf("unsupported password hash format")
	}
	if parts[0] != "sha256" {
		return false, fmt.Errorf("unsupported password hash algorithm %q", parts[0])
	}
	actual := HashPassword(password, parts[1])
	expected := strings.ToLower(strings.TrimSpace(encoded))
	if len(actual) != len(expected) {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1, nil
}
