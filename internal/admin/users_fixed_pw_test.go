package admin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// TestApplyFixedAdminPassword 验证测试实例固定密码重置（议题 #69）：
// 启动时若配置 admin_password_fix，则旧密码一律作废、固定密码可认证，角色保留。
func TestApplyFixedAdminPassword(t *testing.T) {
	tempDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	store := nosql.NewUserStore(db)

	oldHash, err := HashPasswordWithRandomSalt("old-secret")
	if err != nil {
		t.Fatalf("hash old password: %v", err)
	}
	if err := store.Create(config.AdminUser{
		Username:     "admin",
		PasswordHash: oldHash,
		Role:         RoleAdmin,
	}); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}

	if err := ApplyFixedAdminPassword(store, "admin", "fixed-pw"); err != nil {
		t.Fatalf("apply fixed password: %v", err)
	}

	if _, err := AuthenticateUser(store, "admin", "old-secret"); err == nil {
		t.Fatal("old password should be invalidated after fixed-password reset")
	}
	user, err := AuthenticateUser(store, "admin", "fixed-pw")
	if err != nil {
		t.Fatalf("fixed password should authenticate: %v", err)
	}
	if user.Role != RoleAdmin {
		t.Fatalf("role should be preserved, got %q", user.Role)
	}

	// 重复应用（模拟再次重启）应保持幂等可用。
	if err := ApplyFixedAdminPassword(store, "admin", "fixed-pw"); err != nil {
		t.Fatalf("re-apply fixed password: %v", err)
	}
	if _, err := AuthenticateUser(store, "admin", "fixed-pw"); err != nil {
		t.Fatalf("fixed password should still authenticate after re-apply: %v", err)
	}
}

func TestApplyFixedAdminPasswordGuards(t *testing.T) {
	tempDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	store := nosql.NewUserStore(db)

	if err := ApplyFixedAdminPassword(store, "", "pw"); err == nil {
		t.Fatal("empty username should be rejected")
	}
	if err := ApplyFixedAdminPassword(store, "admin", "  "); err == nil {
		t.Fatal("blank password should be rejected")
	}
	err = ApplyFixedAdminPassword(store, "nobody", "pw")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing user should surface not-found error, got: %v", err)
	}
}
