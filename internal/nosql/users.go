package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// UserStore manages admin users backed by bbolt.
type UserStore struct {
	db *bolt.DB
}

// NewUserStore creates a new bbolt-backed admin user store.
func NewUserStore(db *bolt.DB) *UserStore {
	return &UserStore{db: db}
}

// List returns all admin users.
func (s *UserStore) List() ([]config.AdminUser, error) {
	var users []config.AdminUser
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var u config.AdminUser
			if err := json.Unmarshal(v, &u); err != nil {
				return fmt.Errorf("decode admin user %q: %w", string(k), err)
			}
			users = append(users, u)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	cloned := make([]config.AdminUser, len(users))
	copy(cloned, users)
	return cloned, nil
}

// Get returns a single admin user by username.
func (s *UserStore) Get(username string) (config.AdminUser, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return config.AdminUser{}, errors.New("username must not be empty")
	}
	key := strings.ToLower(username)

	var user config.AdminUser
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(v, &user); err != nil {
			return fmt.Errorf("decode admin user %q: %w", key, err)
		}
		found = true
		return nil
	})
	if err != nil {
		return config.AdminUser{}, err
	}
	if !found {
		return config.AdminUser{}, fmt.Errorf("user %q not found", username)
	}
	return user, nil
}

// Create adds a new admin user.
func (s *UserStore) Create(user config.AdminUser) error {
	user.Username = strings.TrimSpace(user.Username)
	if user.Username == "" {
		return errors.New("username must not be empty")
	}
	if user.PasswordHash == "" {
		return errors.New("password_hash must not be empty")
	}
	if user.Role == "" {
		user.Role = "admin"
	}
	key := strings.ToLower(user.Username)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("user %q already exists", user.Username)
		}
		data, err := json.Marshal(user)
		if err != nil {
			return fmt.Errorf("encode admin user: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Update overwrites an existing admin user.
func (s *UserStore) Update(username string, user config.AdminUser) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username must not be empty")
	}
	key := strings.ToLower(username)

	user.Username = strings.TrimSpace(user.Username)
	if user.Username == "" {
		user.Username = username
	}
	if user.Role == "" {
		user.Role = "admin"
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("user %q not found", username)
		}
		data, err := json.Marshal(user)
		if err != nil {
			return fmt.Errorf("encode admin user: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Delete removes an admin user.
func (s *UserStore) Delete(username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username must not be empty")
	}
	key := strings.ToLower(username)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("user %q not found", username)
		}
		return b.Delete([]byte(key))
	})
}

// SeedDefaultUser writes a default admin user only when the bucket is empty.
// It is safe to call on every startup (idempotent).
func (s *UserStore) SeedDefaultUser(user config.AdminUser) error {
	user.Username = strings.TrimSpace(user.Username)
	if user.Username == "" {
		return errors.New("username must not be empty")
	}
	if user.Role == "" {
		user.Role = "admin"
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		// Only seed if bucket is empty.
		c := b.Cursor()
		k, _ := c.First()
		if k != nil {
			return nil // bucket already has data
		}

		key := strings.ToLower(user.Username)
		data, err := json.Marshal(user)
		if err != nil {
			return fmt.Errorf("encode default admin user: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}
