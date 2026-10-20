package auth

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"

	"github.com/LeXwDeX/LLM-Proxy/internal/clientip"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Principal represents an authenticated client.
type Principal struct {
	Name           string
	AccessKey      string // 客户端的 access_key（唯一标识）
	allowAll       bool
	allowedTargets map[string]struct{}
	allowedList    []string
	ipRules        clientip.Rules
}

// CanAccessIP reports whether the client may connect from this IPv4 address.
// A blank ip_address leaves access unrestricted for existing clients.
func (p *Principal) CanAccessIP(address netip.Addr) bool {
	if p == nil {
		return false
	}
	return len(p.ipRules.Prefixes) == 0 || p.ipRules.Contains(address)
}

func (p *Principal) HasIPRestriction() bool {
	return p != nil && len(p.ipRules.Prefixes) > 0
}

// AllowedTargets returns a copy of the allowed targets list.
func (p *Principal) AllowedTargets() []string {
	cloned := make([]string, len(p.allowedList))
	copy(cloned, p.allowedList)
	return cloned
}

// AllowedTargetsSet returns the normalized allowed targets set directly.
// The returned map must NOT be modified by the caller.
// This avoids the allocation overhead of AllowedTargets() + normalizeAllowed().
func (p *Principal) AllowedTargetsSet() map[string]struct{} {
	return p.allowedTargets
}

// AllowAll indicates whether the principal can access all targets.
func (p *Principal) AllowAll() bool {
	return p.allowAll
}

// CanAccess checks if the principal can access the provided target.
func (p *Principal) CanAccess(target string) bool {
	if p == nil {
		return false
	}
	if p.allowAll {
		return true
	}
	_, ok := p.allowedTargets[strings.ToLower(target)]
	return ok
}

// Clone returns a deep copy of the principal.
func (p *Principal) Clone() *Principal {
	if p == nil {
		return nil
	}
	clone := &Principal{
		Name:        p.Name,
		AccessKey:   p.AccessKey,
		allowAll:    p.allowAll,
		allowedList: make([]string, len(p.allowedList)),
		ipRules: clientip.Rules{
			Canonical: p.ipRules.Canonical,
			Prefixes:  append([]netip.Prefix(nil), p.ipRules.Prefixes...),
		},
	}
	copy(clone.allowedList, p.allowedList)
	if len(p.allowedTargets) > 0 {
		clone.allowedTargets = make(map[string]struct{}, len(p.allowedTargets))
		for k, v := range p.allowedTargets {
			clone.allowedTargets[k] = v
		}
	} else {
		clone.allowedTargets = make(map[string]struct{})
	}
	return clone
}

// Store keeps runtime authentication data.
type Store struct {
	mu      sync.RWMutex
	clients map[string]*Principal
}

// NewStore constructs an empty Store.
func NewStore() *Store {
	return &Store{
		clients: make(map[string]*Principal),
	}
}

// LoadFromConfig replaces the current client map from configuration.
// 旧签名保留（无 targets 视图，池展开为空集），等价于
// LoadFromConfigWithPools(clients, nil)。
func (s *Store) LoadFromConfig(clients []config.Client) error {
	return s.loadFromConfig(clients, nil)
}

// LoadFromConfigWithPools replaces the current client map from configuration,
// expanding each client's allowed_pools into member target names derived from
// the provided targets（授权一池即授权全部成员节点）。
//
// 展开语义：有效集 = AllowedTargets ∪ ∪{成员(t) : t.Pool ∈ AllowedPools}
// （全部小写规范化）；不存在的池名贡献空集、不报错；allowAll 按**原始字段**
// 判空（len(AllowedTargets)==0 && len(AllowedPools)==0），防 [""] 边缘漂移。
func (s *Store) LoadFromConfigWithPools(clients []config.Client, targets []config.Target) error {
	return s.loadFromConfig(clients, buildPoolMemberIndex(targets))
}

// buildPoolMemberIndex 构建 pool 名（小写）→ 成员 target 名集合（小写）索引。
func buildPoolMemberIndex(targets []config.Target) map[string]map[string]struct{} {
	if len(targets) == 0 {
		return nil
	}
	idx := make(map[string]map[string]struct{})
	for _, t := range targets {
		pool := strings.ToLower(strings.TrimSpace(t.Pool))
		name := strings.ToLower(strings.TrimSpace(t.Name))
		if pool == "" || name == "" {
			continue
		}
		set, ok := idx[pool]
		if !ok {
			set = make(map[string]struct{})
			idx[pool] = set
		}
		set[name] = struct{}{}
	}
	return idx
}

func (s *Store) loadFromConfig(clients []config.Client, poolMembers map[string]map[string]struct{}) error {
	if s == nil {
		return errors.New("auth store is nil")
	}
	next := make(map[string]*Principal, len(clients))
	owners := make(map[string]string, len(clients))
	for _, client := range clients {
		key := strings.TrimSpace(client.AccessKey)
		if key == "" {
			return fmt.Errorf("client %q missing access_key", client.Name)
		}
		name := strings.TrimSpace(client.Name)
		if name == "" {
			return errors.New("client name must not be empty")
		}
		if prevOwner, exists := owners[key]; exists {
			return fmt.Errorf("duplicate access_key for clients %q and %q", prevOwner, name)
		}
		owners[key] = name
		ipRules, err := clientip.Parse(client.IPAddress)
		if err != nil {
			return fmt.Errorf("client %q: %w", name, err)
		}

		principal := &Principal{
			Name:           name,
			AccessKey:      key,
			allowAll:       len(client.AllowedTargets) == 0 && len(client.AllowedPools) == 0,
			allowedTargets: make(map[string]struct{}),
			allowedList:    make([]string, 0, len(client.AllowedTargets)),
			ipRules:        ipRules,
		}

		for _, target := range client.AllowedTargets {
			target = strings.ToLower(strings.TrimSpace(target))
			if target == "" {
				continue
			}
			if _, exists := principal.allowedTargets[target]; !exists {
				principal.allowedTargets[target] = struct{}{}
				principal.allowedList = append(principal.allowedList, target)
			}
		}
		// 展开 allowed_pools → 成员 target 名（不存在的池贡献空集）。
		for _, pool := range client.AllowedPools {
			pool = strings.ToLower(strings.TrimSpace(pool))
			if pool == "" {
				continue
			}
			// map 迭代顺序非确定，展开结果排序以保证 allowedList 稳定。
			members := make([]string, 0, len(poolMembers[pool]))
			for member := range poolMembers[pool] {
				members = append(members, member)
			}
			sort.Strings(members)
			for _, member := range members {
				if _, exists := principal.allowedTargets[member]; !exists {
					principal.allowedTargets[member] = struct{}{}
					principal.allowedList = append(principal.allowedList, member)
				}
			}
		}

		next[key] = principal
	}

	s.mu.Lock()
	s.clients = next
	s.mu.Unlock()
	return nil
}

// Authenticate returns the principal associated with the key.
//
// Principal 在 LoadFromConfig 构建完成后不可变（热加载时整表替换而非原地
// 修改），因此直接返回共享指针。旧实现每请求 Clone（深拷贝 allowedTargets
// map），把 AllowedTargetsSet 免分配优化全部抵消。调用方不得修改返回值。
func (s *Store) Authenticate(key string) (*Principal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	principal, ok := s.clients[key]
	if !ok {
		return nil, false
	}
	return principal, true
}
