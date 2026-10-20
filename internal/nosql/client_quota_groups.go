package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// QuotaClientEntry is the persisted value of one client's model-group
// selections. The original (case-preserved) client name is stored alongside
// the groups because the bucket key is lowercased for lookup.
//
// When a client is deleted, the entry is archived instead of removed so the
// personnel quota page keeps the historical row for monthly statistics:
// DeletedAt is set and the identity snapshot fields are copied from the
// client record at deletion time (the client record itself no longer exists).
type QuotaClientEntry struct {
	Name         string              `json:"name"`
	Groups       []config.QuotaGroup `json:"groups"`
	DeletedAt    string              `json:"deleted_at,omitempty"` // RFC3339 UTC; empty = active client
	IPAddress    string              `json:"ip_address,omitempty"`
	JobLevel     string              `json:"job_level,omitempty"`
	ProjectGroup string              `json:"project_group,omitempty"`
}

// ClientQuotaStore persists per-client model-group quota selections in a
// dedicated bucket (BucketClientQuotaGroups), keyed by client name (lowercase).
// Personal daily/weekly/monthly limits stay on the client record; model-group
// configuration is owned exclusively by quota management.
type ClientQuotaStore struct{ db *bolt.DB }

// NewClientQuotaStore creates a new bbolt-backed per-client model-group store.
func NewClientQuotaStore(db *bolt.DB) *ClientQuotaStore { return &ClientQuotaStore{db: db} }

// Get returns the model-group quota selections for one client.
// Returns (nil, nil) when the client has no entry.
func (s *ClientQuotaStore) Get(clientName string) ([]config.QuotaGroup, error) {
	key := clientQuotaKey(clientName)
	var entry QuotaClientEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte(BucketClientQuotaGroups)).Get(key)
		if value == nil {
			return nil
		}
		return json.Unmarshal(value, &entry)
	})
	if err != nil {
		return nil, err
	}
	if entry.DeletedAt != "" {
		return nil, nil // archived (deleted client): no active entry
	}
	return entry.Groups, nil
}

// Validate checks the given group selections without writing anything.
// Used by admin handlers to preserve all-or-nothing semantics across the
// personal-quota (client record) and model-group (this store) writes.
func (s *ClientQuotaStore) Validate(groups []config.QuotaGroup) error {
	if err := validateQuotaGroups(groups); err != nil {
		return err
	}
	return s.db.View(func(tx *bolt.Tx) error {
		return validateClientModelGroups(tx, groups)
	})
}

// Replace atomically replaces one client's model-group selections.
// An empty groups slice removes the entry.
func (s *ClientQuotaStore) Replace(clientName string, groups []config.QuotaGroup) error {
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return errors.New("client name must not be empty")
	}
	if err := validateQuotaGroups(groups); err != nil {
		return err
	}
	key := clientQuotaKey(clientName)
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := validateClientModelGroups(tx, groups); err != nil {
			return err
		}
		b := tx.Bucket([]byte(BucketClientQuotaGroups))
		if len(groups) == 0 {
			return b.Delete(key)
		}
		data, err := json.Marshal(QuotaClientEntry{Name: clientName, Groups: groups})
		if err != nil {
			return fmt.Errorf("encode client quota groups: %w", err)
		}
		return b.Put(key, data)
	})
}

// ReplaceMany atomically writes the same group selections to multiple clients.
// Returns the normalized names of the updated clients.
func (s *ClientQuotaStore) ReplaceMany(names []string, groups []config.QuotaGroup) ([]string, error) {
	if err := validateQuotaGroups(groups); err != nil {
		return nil, err
	}
	var updatedNames []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := validateClientModelGroups(tx, groups); err != nil {
			return err
		}
		b := tx.Bucket([]byte(BucketClientQuotaGroups))
		seen := make(map[string]struct{}, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("client name must not be empty")
			}
			key := clientQuotaKey(name)
			if _, duplicate := seen[string(key)]; duplicate {
				return fmt.Errorf("duplicate client %q", name)
			}
			seen[string(key)] = struct{}{}
			if len(groups) == 0 {
				if err := b.Delete(key); err != nil {
					return err
				}
			} else {
				data, err := json.Marshal(QuotaClientEntry{Name: name, Groups: groups})
				if err != nil {
					return fmt.Errorf("encode client quota groups: %w", err)
				}
				if err := b.Put(key, data); err != nil {
					return err
				}
			}
			updatedNames = append(updatedNames, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updatedNames, nil
}

// List returns all per-client group selections keyed by the stored client name.
// Archived entries (deleted clients) are skipped; use ListEntries to include them.
func (s *ClientQuotaStore) List() (map[string][]config.QuotaGroup, error) {
	result := map[string][]config.QuotaGroup{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketClientQuotaGroups)).ForEach(func(_, v []byte) error {
			var entry QuotaClientEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return fmt.Errorf("decode client quota groups: %w", err)
			}
			if entry.DeletedAt != "" {
				return nil
			}
			result[entry.Name] = cloneQuotaGroups(entry.Groups)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Delete removes one client's model-group selections entirely (used by the
// clear-record action; client deletion goes through Archive).
func (s *ClientQuotaStore) Delete(clientName string) error {
	if strings.TrimSpace(clientName) == "" {
		return errors.New("client name must not be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketClientQuotaGroups)).Delete(clientQuotaKey(clientName))
	})
}

// writeArchivedEntry archives client's quota entry in a single write tx,
// unconditionally writing a snapshot (empty groups when no prior entry exists).
func writeArchivedEntry(tx *bolt.Tx, client config.Client) error {
	qb := tx.Bucket([]byte(BucketClientQuotaGroups))
	if qb == nil {
		return errors.New("client quota groups bucket not found")
	}
	clientName := strings.TrimSpace(client.Name)
	key := clientQuotaKey(clientName)
	entry := QuotaClientEntry{Name: clientName}
	if value := qb.Get(key); value != nil {
		if err := json.Unmarshal(value, &entry); err != nil {
			return fmt.Errorf("decode client quota groups for %q: %w", clientName, err)
		}
	}
	entry.DeletedAt = time.Now().UTC().Format(time.RFC3339)
	entry.IPAddress = strings.TrimSpace(client.IPAddress)
	entry.JobLevel = strings.TrimSpace(client.JobLevel)
	entry.ProjectGroup = strings.TrimSpace(client.ProjectGroup)
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode archived client quota groups: %w", err)
	}
	return qb.Put(key, data)
}

// Archive marks one client's model-group selections as archived, preserving
// the quota config and an identity snapshot for monthly statistics after the
// client record is deleted. It writes a snapshot even when the client has no
// prior quota entry (empty groups), so personal-quota-only clients also keep a
// historical row.
func (s *ClientQuotaStore) Archive(client config.Client) error {
	if strings.TrimSpace(client.Name) == "" {
		return errors.New("client name must not be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return writeArchivedEntry(tx, client)
	})
}

// putQuotaEntry writes (or deletes when empty) a client's model-group
// selections in the client_quota_groups bucket. Caller must hold a write tx.
func putQuotaEntry(tx *bolt.Tx, clientName string, groups []config.QuotaGroup) error {
	qb := tx.Bucket([]byte(BucketClientQuotaGroups))
	if qb == nil {
		return errors.New("client quota groups bucket not found")
	}
	if len(groups) == 0 {
		return qb.Delete(clientQuotaKey(clientName))
	}
	data, err := json.Marshal(QuotaClientEntry{Name: clientName, Groups: groups})
	if err != nil {
		return fmt.Errorf("encode client quota groups: %w", err)
	}
	return qb.Put(clientQuotaKey(clientName), data)
}

// UpdateQuota atomically updates one client's job-level binding (clients
// bucket) and plan — its model-group selections with personal coefficients
// (client_quota_groups bucket) — in a single transaction. 额度体系 v2：个人
// 日/周/月折算额度已退役，个人维度限额由职级系数兜底；空 jobLevel 表示解绑
// （未绑职级 = 未配置额度 = 请求拒绝，UI 侧有警示）。非空 jobLevel 必须
// 存在于职级目录。
func (s *ClientQuotaStore) UpdateQuota(clientName string, jobLevel string, groups []config.QuotaGroup) error {
	if err := validateQuotaGroups(groups); err != nil {
		return err
	}
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return errors.New("client name must not be empty")
	}
	jobLevel = strings.TrimSpace(jobLevel)
	return s.db.Update(func(tx *bolt.Tx) error {
		cb := tx.Bucket([]byte(BucketClients))
		if cb == nil {
			return errors.New("clients bucket not found")
		}
		key := []byte(strings.ToLower(clientName))
		value := cb.Get(key)
		if value == nil {
			return fmt.Errorf("client %q not found", clientName)
		}
		if jobLevel != "" {
			if jb := tx.Bucket([]byte(BucketJobLevels)); jb == nil || jb.Get([]byte(strings.ToLower(jobLevel))) == nil {
				return fmt.Errorf("job level %q not found", jobLevel)
			}
		}
		var client config.Client
		if err := json.Unmarshal(value, &client); err != nil {
			return fmt.Errorf("decode client %q: %w", clientName, err)
		}
		client.JobLevel = jobLevel
		data, err := json.Marshal(client)
		if err != nil {
			return fmt.Errorf("encode client %q: %w", clientName, err)
		}
		if err := cb.Put(key, data); err != nil {
			return err
		}
		if err := validateClientModelGroups(tx, groups); err != nil {
			return err
		}
		return putQuotaEntry(tx, clientName, groups)
	})
}

// QuotaSyncSelectors selects the union of active clients to receive a source plan.
// Pointer fields distinguish an omitted selector from an explicitly empty one.
type QuotaSyncSelectors struct {
	Clients      []string `json:"clients"`
	ProjectGroup *string  `json:"project_group"`
	JobLevel     *string  `json:"job_level"`
}

var ErrQuotaSyncSourceNotFound = errors.New("client not found")

// QuotaSyncValidationError identifies an invalid sync command, as distinct from
// database or decoding failures that must not be exposed in an HTTP response.
type QuotaSyncValidationError struct{ err error }

func (e *QuotaSyncValidationError) Error() string { return e.err.Error() }
func (e *QuotaSyncValidationError) Unwrap() error { return e.err }

func invalidQuotaSync(err error) error { return &QuotaSyncValidationError{err: err} }

// SyncQuota reads the active source, its complete plan and selector membership,
// then validates and replaces every target plan in the same write transaction.
// Identity, job level, authorization and usage records are never copied.
// Only names actually written are returned, in canonical sorted order.
func (s *ClientQuotaStore) SyncQuota(source string, selectors QuotaSyncSelectors) ([]string, error) {
	var updatedNames []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		cb := tx.Bucket([]byte(BucketClients))
		if cb == nil {
			return errors.New("clients bucket not found")
		}
		value := cb.Get(clientQuotaKey(source))
		if value == nil {
			return ErrQuotaSyncSourceNotFound
		}
		var src config.Client
		if err := json.Unmarshal(value, &src); err != nil {
			return fmt.Errorf("decode source client: %w", err)
		}
		// The source path has always required the stored spelling. Targets use
		// case-insensitive storage lookup and are returned with stored spelling.
		if src.Name != source {
			return ErrQuotaSyncSourceNotFound
		}
		if selectors.JobLevel != nil && strings.TrimSpace(*selectors.JobLevel) == "" {
			return invalidQuotaSync(errors.New("job_level target must not be empty"))
		}
		qb := tx.Bucket([]byte(BucketClientQuotaGroups))
		if qb == nil {
			return errors.New("client quota groups bucket not found")
		}
		var groups []config.QuotaGroup
		if value := qb.Get(clientQuotaKey(src.Name)); value != nil {
			var entry QuotaClientEntry
			if err := json.Unmarshal(value, &entry); err != nil {
				return fmt.Errorf("decode source quota groups: %w", err)
			}
			if entry.DeletedAt == "" {
				groups = entry.Groups
			}
		}
		targets := make(map[string]string)
		addTarget := func(name string) {
			name = strings.TrimSpace(name)
			if name != "" && !strings.EqualFold(name, src.Name) {
				targets[string(clientQuotaKey(name))] = name
			}
		}
		for _, name := range selectors.Clients {
			addTarget(name)
		}
		if selectors.ProjectGroup != nil || selectors.JobLevel != nil {
			if err := cb.ForEach(func(_, value []byte) error {
				var client config.Client
				if err := json.Unmarshal(value, &client); err != nil {
					return fmt.Errorf("decode sync candidate: %w", err)
				}
				if selectors.ProjectGroup != nil {
					want, got := strings.TrimSpace(*selectors.ProjectGroup), strings.TrimSpace(client.ProjectGroup)
					if want == "" {
						want = "未分组"
					}
					if got == "" {
						got = "未分组"
					}
					if want == got {
						addTarget(client.Name)
					}
				}
				if selectors.JobLevel != nil && strings.EqualFold(strings.TrimSpace(client.JobLevel), strings.TrimSpace(*selectors.JobLevel)) {
					addTarget(client.Name)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if len(targets) == 0 {
			return invalidQuotaSync(errors.New("no sync targets resolved"))
		}
		if err := validateQuotaGroups(groups); err != nil {
			return invalidQuotaSync(err)
		}
		if err := validateClientModelGroups(tx, groups); err != nil {
			return invalidQuotaSync(err)
		}
		keys := make([]string, 0, len(targets))
		for key := range targets {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := cb.Get([]byte(key))
			if value == nil {
				return invalidQuotaSync(fmt.Errorf("client %q not found", targets[key]))
			}
			var client config.Client
			if err := json.Unmarshal(value, &client); err != nil {
				return fmt.Errorf("decode sync target: %w", err)
			}
			updatedNames = append(updatedNames, client.Name)
		}
		sort.Strings(updatedNames)
		for _, name := range updatedNames {
			if err := putQuotaEntry(tx, name, groups); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updatedNames, nil
}

// DeleteClientAndArchive removes a client record and archives its quota entry
// in a single transaction (the archived snapshot keeps the historical row on
// the personnel quota page).
func (s *ClientQuotaStore) DeleteClientAndArchive(client config.Client) error {
	clientName := strings.TrimSpace(client.Name)
	if clientName == "" {
		return errors.New("client name must not be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if cb := tx.Bucket([]byte(BucketClients)); cb != nil {
			if err := cb.Delete([]byte(strings.ToLower(clientName))); err != nil {
				return err
			}
		}
		return writeArchivedEntry(tx, client)
	})
}

// ListEntries returns all entries (active and archived) keyed by the stored
// client name.
func (s *ClientQuotaStore) ListEntries() (map[string]QuotaClientEntry, error) {
	result := map[string]QuotaClientEntry{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketClientQuotaGroups)).ForEach(func(_, v []byte) error {
			var entry QuotaClientEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return fmt.Errorf("decode client quota groups: %w", err)
			}
			result[entry.Name] = QuotaClientEntry{
				Name:         entry.Name,
				Groups:       cloneQuotaGroups(entry.Groups),
				DeletedAt:    entry.DeletedAt,
				IPAddress:    entry.IPAddress,
				JobLevel:     entry.JobLevel,
				ProjectGroup: entry.ProjectGroup,
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func clientQuotaKey(clientName string) []byte {
	return []byte(strings.ToLower(strings.TrimSpace(clientName)))
}

func cloneQuotaGroups(groups []config.QuotaGroup) []config.QuotaGroup {
	return append([]config.QuotaGroup(nil), groups...)
}

func validateQuotaGroups(groups []config.QuotaGroup) error {
	names := map[string]bool{}
	for i, group := range groups {
		name := strings.TrimSpace(group.Name)
		if name == "" || names[strings.ToLower(name)] {
			return fmt.Errorf("quota_groups[%d].name must be unique and non-empty", i)
		}
		if group.Coefficient != nil && *group.Coefficient < 0 {
			return fmt.Errorf("quota_groups[%d].coefficient must not be negative", i)
		}
		names[strings.ToLower(name)] = true
	}
	return nil
}

// bindingKey normalizes a binding into a case-insensitive unique key used for
// catalog uniqueness checks and stale-binding detection. A whole-target binding
// (empty Model) keys as "target\x00"; an exact binding keys as "target\x00model".
func bindingKey(b config.QuotaGroupBinding) string {
	target := strings.ToLower(strings.TrimSpace(b.Target))
	model := strings.ToLower(strings.TrimSpace(b.Model))
	if model == "" {
		return target + "\x00"
	}
	return target + "\x00" + model
}

func validateQuotaGroupBindings(tx *bolt.Tx, bindings []config.QuotaGroupBinding, staleAllowed map[string]bool) error {
	targets := tx.Bucket([]byte(BucketTargets))
	for i, binding := range bindings {
		targetName := strings.TrimSpace(binding.Target)
		value := targets.Get([]byte(strings.ToLower(targetName)))
		if value == nil {
			if staleAllowed != nil && staleAllowed[bindingKey(binding)] {
				// Pre-existing stale binding preserved on update.
				continue
			}
			return fmt.Errorf("bindings[%d] target %q not found", i, targetName)
		}
		model := strings.TrimSpace(binding.Model)
		if model == "" {
			continue
		}
		var target config.Target
		if err := json.Unmarshal(value, &target); err != nil {
			return fmt.Errorf("decode target %q: %w", targetName, err)
		}
		found := false
		for _, mapping := range target.ModelMappings {
			if strings.EqualFold(strings.TrimSpace(mapping.Upstream), model) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("bindings[%d] model %q not found in target %q upstream models", i, model, targetName)
		}
	}
	return nil
}

func validateClientModelGroups(tx *bolt.Tx, groups []config.QuotaGroup) error {
	b := tx.Bucket([]byte(BucketModelGroups))
	for i, group := range groups {
		name := strings.TrimSpace(group.Name)
		if name == "" || strings.EqualFold(name, "company_general") {
			continue
		}
		if b == nil || b.Get([]byte(strings.ToLower(name))) == nil {
			return fmt.Errorf("quota_groups[%d] model group %q not found", i, name)
		}
	}
	return nil
}
