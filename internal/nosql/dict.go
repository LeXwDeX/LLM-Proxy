package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// Dictionary meta keys stored in BucketMeta. Each value is a JSON array of
// sorted strings. The job-level and project-group dictionaries share the same
// accessor functions; no dedicated bucket is required.
const (
	MetaKeyJobLevels     = "dict_job_levels"
	MetaKeyProjectGroups = "dict_project_groups"
)

// DictStore persists ordered string dictionaries (job levels, project groups)
// inside the shared meta bucket. One store serves all dictionaries.
type DictStore struct{ db *bolt.DB }

func NewDictStore(db *bolt.DB) *DictStore { return &DictStore{db: db} }

// List returns the sorted entries of the dictionary stored under metaKey.
func (s *DictStore) List(metaKey string) ([]string, error) {
	var entries []string
	err := s.db.View(func(tx *bolt.Tx) error {
		var readErr error
		entries, readErr = readDictEntries(tx, metaKey)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Create appends a new entry to the dictionary. Entries are unique
// case-insensitively and sorted on write.
func (s *DictStore) Create(metaKey, name string) error {
	name = strings.TrimSpace(name)
	if err := validateDictEntryName(name); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		entries, err := readDictEntries(tx, metaKey)
		if err != nil {
			return err
		}
		if dictIndex(entries, name) >= 0 {
			return fmt.Errorf("entry %q already exists", name)
		}
		entries = append(entries, name)
		return writeDictEntries(tx, metaKey, entries)
	})
}

// Update renames an existing dictionary entry.
func (s *DictStore) Update(metaKey, oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if err := validateDictEntryName(newName); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		entries, err := readDictEntries(tx, metaKey)
		if err != nil {
			return err
		}
		idx := dictIndex(entries, oldName)
		if idx < 0 {
			return fmt.Errorf("entry %q not found", oldName)
		}
		if existing := dictIndex(entries, newName); existing >= 0 && existing != idx {
			return fmt.Errorf("entry %q already exists", newName)
		}
		entries[idx] = newName
		return writeDictEntries(tx, metaKey, entries)
	})
}

// Delete removes an entry from the dictionary. Client/template records
// referencing the removed value are left untouched; the admin UI keeps
// showing the current value as an extra option when editing.
func (s *DictStore) Delete(metaKey, name string) error {
	name = strings.TrimSpace(name)
	return s.db.Update(func(tx *bolt.Tx) error {
		entries, err := readDictEntries(tx, metaKey)
		if err != nil {
			return err
		}
		idx := dictIndex(entries, name)
		if idx < 0 {
			return fmt.Errorf("entry %q not found", name)
		}
		entries = append(entries[:idx], entries[idx+1:]...)
		return writeDictEntries(tx, metaKey, entries)
	})
}

func validateDictEntryName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	return nil
}

func readDictEntries(tx *bolt.Tx, metaKey string) ([]string, error) {
	b := tx.Bucket([]byte(BucketMeta))
	if b == nil {
		return nil, nil
	}
	raw := b.Get([]byte(metaKey))
	if raw == nil {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode dictionary %q: %w", metaKey, err)
	}
	return entries, nil
}

func writeDictEntries(tx *bolt.Tx, metaKey string, entries []string) error {
	b := tx.Bucket([]byte(BucketMeta))
	if b == nil {
		return errors.New("meta bucket not found")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i] < entries[j] })
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return b.Put([]byte(metaKey), data)
}

func dictIndex(entries []string, name string) int {
	for i, entry := range entries {
		if strings.EqualFold(entry, name) {
			return i
		}
	}
	return -1
}
