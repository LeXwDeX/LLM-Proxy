package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
)

// ErrTargetRenameConflict 表示改名/合并与现有目标、价表、模型组绑定或历史用量冲突；库内容不变。
var ErrTargetRenameConflict = errors.New("target rename conflict")

// ErrTargetNotFound 表示改名/合并的源目标（或合并的目的目标）不存在；库内容不变。
var ErrTargetNotFound = errors.New("target not found")

// TargetRenameRequest 描述一次受支持的目标改名（To 不存在）或合并（Merge=true，To 已存在）。
// ExpectedETag 非 nil 时，源目标当前存储字节的 ETag 必须相等，否则 ErrTargetETagConflict。
type TargetRenameRequest struct {
	From         string
	To           string
	Merge        bool
	ExpectedETag *string
}

// TargetRenameResult 列出同一事务内被同步的按名绑定对象（均为原始显示名，已排序）。
type TargetRenameResult struct {
	From             string   `json:"from"`
	To               string   `json:"to"`
	Merged           bool     `json:"merged"`
	PricingTemplates []string `json:"pricing_templates"`
	ModelGroups      []string `json:"model_groups"`
	Clients          []string `json:"clients"`
}

// Rename 是唯一受支持的目标改名/合并路径（Issue #195）。一个 bbolt 写事务内：
//
//   - 改名 From→To（To 不存在）：目标记录与顺序改名；凡绑定 From 的价表追加 To，
//     凡绑定 From 的模型组绑定追加同模型的 To 绑定——From 一律保留，使按写入时
//     target 名记账的历史事件与额度聚合继续命中原价表与原模型组。To 若在用量或
//     额度聚合中已有历史（曾被已删除目标使用），拒绝，避免追加绑定改写那段历史的计价；
//   - 合并 From→To（Merge，To 已存在）：删除目标 From，To 的配置与绑定不变；
//     From 的价表与模型组绑定原样保留，供历史计价与归属使用；
//   - 两种情况下客户端 allowed_targets 中的 From 都替换为 To（同一上游接替，
//     且避免移除后列表变空被解释为“全部放行”）。
//
// 不写 usage_events、用量/额度聚合或任何其他历史账本。任何冲突整体回滚。
func (s *TargetStore) Rename(req TargetRenameRequest) (TargetRenameResult, error) {
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if from == "" || to == "" {
		return TargetRenameResult{}, errors.New("from and to must not be empty")
	}
	if strings.ContainsRune(to, 0) {
		return TargetRenameResult{}, errors.New("invalid target name")
	}
	fromKey, toKey := strings.ToLower(from), strings.ToLower(to)
	if req.Merge && fromKey == toKey {
		return TargetRenameResult{}, fmt.Errorf("%w: cannot merge target %q into itself", ErrTargetRenameConflict, from)
	}
	result := TargetRenameResult{To: to, Merged: req.Merge, PricingTemplates: []string{}, ModelGroups: []string{}, Clients: []string{}}

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		raw := b.Get([]byte(fromKey))
		if raw == nil {
			if req.ExpectedETag != nil {
				return ErrTargetETagConflict
			}
			return fmt.Errorf("%w: %q", ErrTargetNotFound, from)
		}
		if req.ExpectedETag != nil && targetETagOf(raw) != *req.ExpectedETag {
			return ErrTargetETagConflict
		}
		record := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("decode target %q: %w", from, err)
		}
		var storedName string
		if err := json.Unmarshal(record["name"], &storedName); err != nil {
			return fmt.Errorf("decode target %q name: %w", from, err)
		}
		result.From = strings.TrimSpace(storedName)
		if result.From == "" {
			result.From = from
		}
		destExists := toKey != fromKey && b.Get([]byte(toKey)) != nil
		switch {
		case req.Merge && !destExists:
			return fmt.Errorf("%w: merge destination %q", ErrTargetNotFound, to)
		case !req.Merge && destExists:
			return fmt.Errorf("%w: target %q already exists; use merge", ErrTargetRenameConflict, to)
		}
		if !req.Merge && toKey != fromKey {
			used, err := targetNameHasUsageHistory(tx, toKey)
			if err != nil {
				return err
			}
			if used {
				return fmt.Errorf("%w: target name %q already has usage history; choose another name", ErrTargetRenameConflict, to)
			}
		}

		if req.Merge {
			if err := b.Delete([]byte(fromKey)); err != nil {
				return err
			}
			if err := updateTargetOrder(tx, func(order []string) []string {
				next := order[:0]
				for _, key := range order {
					if key != fromKey {
						next = append(next, key)
					}
				}
				return next
			}); err != nil {
				return err
			}
		} else {
			var target config.Target
			if err := json.Unmarshal(raw, &target); err != nil {
				return fmt.Errorf("decode target %q: %w", from, err)
			}
			target.Name = to
			if err := validateTarget(target); err != nil {
				return err
			}
			encodedName, err := json.Marshal(to)
			if err != nil {
				return err
			}
			// Edit only the name so unknown/legacy fields and raw custom_body
			// values survive byte-for-byte semantics.
			record["name"] = encodedName
			data, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode target: %w", err)
			}
			if toKey != fromKey {
				if err := b.Delete([]byte(fromKey)); err != nil {
					return err
				}
			}
			if err := b.Put([]byte(toKey), data); err != nil {
				return err
			}
			if err := updateTargetOrder(tx, func(order []string) []string {
				for i, key := range order {
					if key == fromKey {
						order[i] = toKey
					}
				}
				return dedupeTargetOrder(order)
			}); err != nil {
				return err
			}
		}

		if !req.Merge && toKey != fromKey {
			templates, err := extendPricingTemplateTargets(tx, fromKey, to)
			if err != nil {
				return err
			}
			result.PricingTemplates = templates
			groups, err := extendModelGroupBindings(tx, fromKey, to)
			if err != nil {
				return err
			}
			result.ModelGroups = groups
		}
		if toKey != fromKey {
			clients, err := renameClientAllowedTargets(tx, fromKey, toKey)
			if err != nil {
				return err
			}
			result.Clients = clients
		}
		return nil
	})
	if err != nil {
		return TargetRenameResult{}, err
	}
	return result, nil
}

// targetNameHasUsageHistory 报告 targetKey（小写）是否已作为写入时 target 名出现在
// 额度聚合或用量聚合中。只读扫描键，不解码单元。
func targetNameHasUsageHistory(tx *bolt.Tx, targetKey string) (bool, error) {
	if b := tx.Bucket([]byte(BucketQuotaUsageAggHourly)); b != nil {
		found := false
		if err := b.ForEach(func(k, _ []byte) error {
			// hour|client|target|model, each dimension quotaKeyEncode'd.
			parts := strings.Split(string(k), "|")
			if len(parts) == 4 && strings.ToLower(quotaKeyDecode(parts[2])) == targetKey {
				found = true
				return errStopScan
			}
			return nil
		}); err != nil && !errors.Is(err, errStopScan) {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	if b := tx.Bucket([]byte(BucketUsageAggHourly)); b != nil {
		found := false
		if err := b.ForEach(func(k, _ []byte) error {
			entry, err := decodeAggKey(k)
			if err == nil && entry.dimsOK && strings.ToLower(strings.TrimSpace(entry.target)) == targetKey {
				found = true
				return errStopScan
			}
			return nil
		}); err != nil && !errors.Is(err, errStopScan) {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

var errStopScan = errors.New("stop scan")

// RenameClientAllowedTarget 返回 client 的 allowed_targets 中 from 替换为 to（大小写不敏感、
// 去重）后的副本；changed 表示是否引用 from。与 Rename 的事务内改写同一口径，供管理端在
// 落库前用新客户端集合校验 auth 展开。
func RenameClientAllowedTarget(client config.Client, from, to string) (config.Client, bool) {
	fromKey, toKey := strings.ToLower(strings.TrimSpace(from)), strings.ToLower(strings.TrimSpace(to))
	next, changed := renameAllowedTargetList(client.AllowedTargets, fromKey, toKey)
	if !changed {
		return client, false
	}
	client.AllowedTargets = next
	return client, true
}

func renameAllowedTargetList(list []string, fromKey, toKey string) ([]string, bool) {
	changed := false
	next := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == fromKey {
			key, changed = toKey, true
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		next = append(next, key)
	}
	return next, changed
}

func renameClientAllowedTargets(tx *bolt.Tx, fromKey, toKey string) ([]string, error) {
	b := tx.Bucket([]byte(BucketClients))
	if b == nil {
		return []string{}, nil
	}
	type write struct{ key, data []byte }
	var writes []write
	names := []string{}
	if err := b.ForEach(func(k, v []byte) error {
		record := map[string]json.RawMessage{}
		if err := json.Unmarshal(v, &record); err != nil {
			return fmt.Errorf("decode client %q: %w", string(k), err)
		}
		var allowed []string
		if rawAllowed, ok := record["allowed_targets"]; ok && len(rawAllowed) > 0 {
			if err := json.Unmarshal(rawAllowed, &allowed); err != nil {
				return fmt.Errorf("decode client %q allowed_targets: %w", string(k), err)
			}
		}
		next, changed := renameAllowedTargetList(allowed, fromKey, toKey)
		if !changed {
			return nil
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		record["allowed_targets"] = encoded
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		var name string
		_ = json.Unmarshal(record["name"], &name)
		if strings.TrimSpace(name) == "" {
			name = string(k)
		}
		names = append(names, name)
		writes = append(writes, write{append([]byte(nil), k...), data})
		return nil
	}); err != nil {
		return nil, err
	}
	for _, w := range writes {
		if err := b.Put(w.key, w.data); err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

// extendPricingTemplateTargets 给每个绑定 fromKey 的价表追加 to（保留原绑定与模板主键），
// 然后对全量模板做覆盖校验；冲突（如 to 已被其他模板以重叠时段绑定）整体拒绝。
func extendPricingTemplateTargets(tx *bolt.Tx, fromKey, to string) ([]string, error) {
	b := tx.Bucket([]byte(BucketPricingTemplates))
	if b == nil {
		return []string{}, nil
	}
	toKey := strings.ToLower(to)
	type write struct {
		key  []byte
		tmpl pricing.PricingTemplate
	}
	var writes []write
	var all []pricing.PricingTemplate
	ids := []string{}
	seenIDs := map[string]bool{}
	if err := b.ForEach(func(k, v []byte) error {
		var t pricing.PricingTemplate
		if err := json.Unmarshal(v, &t); err != nil {
			return fmt.Errorf("decode pricing template %q: %w", string(k), err)
		}
		targets := t.EffectiveTargets()
		bindsFrom, bindsTo := false, false
		for _, target := range targets {
			switch strings.ToLower(target) {
			case fromKey:
				bindsFrom = true
			case toKey:
				bindsTo = true
			}
		}
		if bindsFrom && !bindsTo {
			// Appending keeps Targets[0] (and therefore the storage key) unchanged.
			t.Targets = append(append([]string(nil), targets...), to)
			pricing.NormalizeTemplate(&t)
			writes = append(writes, write{append([]byte(nil), k...), t})
			if !seenIDs[t.ID] {
				seenIDs[t.ID] = true
				ids = append(ids, t.ID)
			}
		}
		all = append(all, t)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(writes) == 0 {
		return ids, nil
	}
	// Duplicate keys carrying the same ID (drifted legacy rows) count once.
	unique := make([]pricing.PricingTemplate, 0, len(all))
	counted := map[string]bool{}
	for _, t := range all {
		if t.ID != "" {
			if counted[t.ID] {
				continue
			}
			counted[t.ID] = true
		}
		unique = append(unique, t)
	}
	if err := pricing.ValidateCoverageSet(unique); err != nil {
		return nil, fmt.Errorf("%w: pricing bindings for %q: %v", ErrTargetRenameConflict, to, err)
	}
	for _, w := range writes {
		data, err := json.Marshal(w.tmpl)
		if err != nil {
			return nil, fmt.Errorf("encode pricing template: %w", err)
		}
		if err := b.Put(w.key, data); err != nil {
			return nil, err
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// extendModelGroupBindings 给每个绑定 fromKey 的模型组追加同模型的 to 绑定（保留原绑定），
// 并按目录级唯一性校验新增绑定（一个 target+model 或整目标绑定只能属于一个模型组）。
func extendModelGroupBindings(tx *bolt.Tx, fromKey, to string) ([]string, error) {
	b := tx.Bucket([]byte(BucketModelGroups))
	if b == nil {
		return []string{}, nil
	}
	type groupRecord struct {
		key      []byte
		record   map[string]json.RawMessage
		name     string
		bindings []config.QuotaGroupBinding
		added    []config.QuotaGroupBinding
	}
	var groups []*groupRecord
	if err := b.ForEach(func(k, v []byte) error {
		record := map[string]json.RawMessage{}
		if err := json.Unmarshal(v, &record); err != nil {
			return fmt.Errorf("decode model group %q: %w", string(k), err)
		}
		g := &groupRecord{key: append([]byte(nil), k...), record: record}
		_ = json.Unmarshal(record["name"], &g.name)
		if rawBindings, ok := record["bindings"]; ok && len(rawBindings) > 0 {
			if err := json.Unmarshal(rawBindings, &g.bindings); err != nil {
				return fmt.Errorf("decode model group %q bindings: %w", string(k), err)
			}
		}
		present := map[string]bool{}
		for _, binding := range g.bindings {
			present[bindingKey(binding)] = true
		}
		for _, binding := range g.bindings {
			if strings.ToLower(strings.TrimSpace(binding.Target)) != fromKey {
				continue
			}
			copyBinding := config.QuotaGroupBinding{Target: to, Model: binding.Model}
			if present[bindingKey(copyBinding)] {
				continue
			}
			present[bindingKey(copyBinding)] = true
			g.added = append(g.added, copyBinding)
		}
		g.bindings = append(g.bindings, g.added...)
		groups = append(groups, g)
		return nil
	}); err != nil {
		return nil, err
	}
	// Only the copies added here are checked: pre-existing data was already
	// validated by ModelGroupStore writes and must not block an unrelated rename.
	for _, g := range groups {
		for _, binding := range g.added {
			for _, other := range groups {
				if other == g {
					continue
				}
				for _, theirs := range other.bindings {
					if bindingKey(theirs) == bindingKey(binding) {
						return nil, fmt.Errorf("%w: binding %q/%q already used by model group %q", ErrTargetRenameConflict, binding.Target, binding.Model, other.name)
					}
				}
			}
		}
	}
	names := []string{}
	for _, g := range groups {
		if len(g.added) == 0 {
			continue
		}
		encoded, err := json.Marshal(g.bindings)
		if err != nil {
			return nil, err
		}
		g.record["bindings"] = encoded
		data, err := json.Marshal(g.record)
		if err != nil {
			return nil, err
		}
		if err := b.Put(g.key, data); err != nil {
			return nil, err
		}
		names = append(names, g.name)
	}
	sort.Strings(names)
	return names, nil
}
