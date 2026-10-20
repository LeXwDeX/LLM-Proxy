package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestUserStoreCRUD(t *testing.T) {
	db := testDB(t)
	store := NewUserStore(db)

	user := config.AdminUser{
		Username:     "admin",
		PasswordHash: "sha256$salt$hash",
		Role:         "admin",
	}

	// Create.
	if err := store.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Get.
	got, err := store.Get("admin")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "admin" || got.PasswordHash != "sha256$salt$hash" {
		t.Fatalf("unexpected user: %+v", got)
	}

	// List.
	users, err := store.List()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}

	// Update.
	updated := config.AdminUser{
		Username:     "admin",
		PasswordHash: "sha256$salt$newhash",
		Role:         "superadmin",
	}
	if err := store.Update("admin", updated); err != nil {
		t.Fatalf("update user: %v", err)
	}
	got, err = store.Get("admin")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Role != "superadmin" || got.PasswordHash != "sha256$salt$newhash" {
		t.Fatalf("unexpected updated user: %+v", got)
	}

	// Delete.
	if err := store.Delete("admin"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	_, err = store.Get("admin")
	if err == nil {
		t.Fatalf("expected error after delete")
	}
}

func TestUserStoreSeedDefaultUser(t *testing.T) {
	db := testDB(t)
	store := NewUserStore(db)

	user := config.AdminUser{
		Username:     "admin",
		PasswordHash: "sha256$salt$hash",
		Role:         "admin",
	}

	// Seed into empty bucket should succeed.
	if err := store.SeedDefaultUser(user); err != nil {
		t.Fatalf("seed: %v", err)
	}

	users, err := store.List()
	if err != nil {
		t.Fatalf("list after seed: %v", err)
	}
	if len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("unexpected users after seed: %+v", users)
	}

	// Seed again should be idempotent (no duplicate).
	if err := store.SeedDefaultUser(config.AdminUser{
		Username:     "admin2",
		PasswordHash: "sha256$salt$hash2",
		Role:         "admin",
	}); err != nil {
		t.Fatalf("seed again: %v", err)
	}

	users, err = store.List()
	if err != nil {
		t.Fatalf("list after second seed: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user after idempotent seed, got %d", len(users))
	}
}

func TestUserStoreDuplicateUsername(t *testing.T) {
	db := testDB(t)
	store := NewUserStore(db)

	user := config.AdminUser{
		Username:     "admin",
		PasswordHash: "sha256$salt$hash",
		Role:         "admin",
	}
	if err := store.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Duplicate username (case-insensitive).
	user2 := config.AdminUser{
		Username:     "ADMIN",
		PasswordHash: "sha256$salt$hash2",
		Role:         "admin",
	}
	if err := store.Create(user2); err == nil {
		t.Fatalf("expected duplicate username error")
	}
}
