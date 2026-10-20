// Command reset-admin-password-stdin resets the password of an existing,
// enabled admin account directly in the bbolt database.
//
// Usage:
//
//	go run ./scripts/reset-admin-password-stdin -db <llms-proxy.db> [-user <account>] [-list]
//
// The new password is read from stdin (never argv or env) and must be at
// least 32 bytes. Unlike the admin API, this tool writes the hash directly
// into the database and cannot run the interactive self-service checks, so
// it enforces a stricter offline minimum. A trailing newline is optional:
// both `printf '%s' pw |` and `printf '%s\n' pw |` work.
//
// The llms-proxy service must be stopped first: bbolt allows a single writer
// and the open below fails after a 3s timeout while the service holds the
// lock. The account is never created; only existing, enabled admin accounts
// are updated. For bootstrapping the very first account use
// scripts/reset-admin-password instead.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

const bucketAdminUsers = "admin_users"

func main() {
	dbPath := flag.String("db", "", "bbolt database path")
	username := flag.String("user", "admin", "existing admin account")
	list := flag.Bool("list", false, "list account names and roles")
	flag.Parse()
	if strings.TrimSpace(*dbPath) == "" {
		fatal(errors.New("-db is required"))
	}
	db, err := bolt.Open(*dbPath, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		fatal(fmt.Errorf("open bbolt: %w", err))
	}
	defer db.Close()
	if *list {
		if err := db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(bucketAdminUsers))
			if bucket == nil {
				return errors.New("admin_users bucket missing")
			}
			return bucket.ForEach(func(key, value []byte) error {
				var account config.AdminUser
				if err := json.Unmarshal(value, &account); err != nil {
					return err
				}
				fmt.Printf("key=%s username=%s role=%s disabled=%t\n",
					string(key), account.Username, account.Role, account.Disabled)
				return nil
			})
		}); err != nil {
			fatal(err)
		}
		return
	}
	// Accept both a newline-terminated line and an EOF-terminated final
	// line so generated passwords piped without a trailing newline work.
	newPassword, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
	if readErr != nil && !(errors.Is(readErr, io.EOF) && len(newPassword) > 0) {
		fatal(fmt.Errorf("read password: %w", readErr))
	}
	password := strings.TrimRight(string(newPassword), "\r\n")
	if len(password) < 32 {
		fatal(errors.New("password from stdin must be at least 32 bytes"))
	}
	newHash, err := admin.HashPasswordWithRandomSalt(password)
	if err != nil {
		fatal(fmt.Errorf("hash password: %w", err))
	}
	key := strings.ToLower(strings.TrimSpace(*username))
	if key == "" {
		fatal(errors.New("-user is required"))
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAdminUsers))
		if bucket == nil {
			return errors.New("admin_users bucket missing")
		}
		data := bucket.Get([]byte(key))
		if data == nil {
			return fmt.Errorf("existing account %q not found", key)
		}
		var account config.AdminUser
		if err := json.Unmarshal(data, &account); err != nil {
			return err
		}
		if account.Role != admin.RoleAdmin || account.Disabled {
			return fmt.Errorf("account %q is not an enabled admin", key)
		}
		account.PasswordHash = newHash
		account.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		updated, err := json.Marshal(account)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), updated)
	}); err != nil {
		fatal(err)
	}
	fmt.Println("password reset completed")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
