package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// One validating scanner serves every top-level JSON edit in this package:
// HTTP model rewrite and include_usage insertion, the WebSocket request and
// event edits, and protected-member normalization. It accepts exactly what
// json.Valid accepts for an object (grammar, whitespace, invalid UTF-8 inside
// strings and the 10000 nesting limit of encoding/json) and records each
// top-level member's raw bytes.

// jsonMember is one top-level member of a JSON object, as raw bytes of the
// scanned data plus their offsets.
type jsonMember struct {
	key   []byte // the key string token, quotes included
	value []byte // the value token
	span  []byte // key through value as written, without separators

	keyStart, valueStart, valueEnd int
}

// decodedKey returns the key as encoding/json decodes it (escapes resolved,
// invalid UTF-8 replaced by U+FFFD).
func (m jsonMember) decodedKey() (string, bool) {
	return jsonString(m.key)
}

// named compares the decoded key exactly, as map decoding does.
func (m jsonMember) named(name string) bool {
	if plainASCIIKey(m.key) {
		return len(m.key) == len(name)+2 && string(m.key[1:len(m.key)-1]) == name
	}
	key, ok := m.decodedKey()
	return ok && key == name
}

// folds compares the decoded key as struct decoding does: Unicode simple case
// folding (bytes.EqualFold), so "Model", "MODEL" and "ſtream" match.
func (m jsonMember) folds(name string) bool {
	if plainASCIIKey(m.key) {
		return len(m.key) == len(name)+2 && strings.EqualFold(string(m.key[1:len(m.key)-1]), name)
	}
	key, ok := m.decodedKey()
	return ok && strings.EqualFold(key, name)
}

// plainASCIIKey reports a key token whose bytes are its decoded value: ASCII
// with no escapes. Only then may it be compared without decoding.
func plainASCIIKey(key []byte) bool {
	for _, c := range key[1 : len(key)-1] {
		if c == '\\' || c >= 0x80 {
			return false
		}
	}
	return true
}

// jsonAll returns the indexes of every member named exactly name, in order.
func jsonAll(members []jsonMember, name string) []int {
	var found []int
	for i, m := range members {
		if m.named(name) {
			found = append(found, i)
		}
	}
	return found
}

// jsonLast returns the raw value of the last member with this exact key, the
// one map decoding keeps, or nil.
func jsonLast(members []jsonMember, name string) []byte {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].named(name) {
			return members[i].value
		}
	}
	return nil
}

// jsonHas reports whether any member has this exact key.
func jsonHas(members []jsonMember, name string) bool {
	return jsonLast(members, name) != nil
}

// jsonLastFold returns the raw value of the last member whose key folds to
// name, the member a struct field would end up with when every value decodes.
func jsonLastFold(members []jsonMember, name string) []byte {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].folds(name) {
			return members[i].value
		}
	}
	return nil
}

// jsonStructString decodes members into a struct string field tagged name
// exactly as encoding/json does: folded keys in order, null leaves the field
// unchanged, and any other non-string value fails the whole decode.
func jsonStructString(members []jsonMember, name string) (string, bool) {
	value := ""
	for _, m := range members {
		if !m.folds(name) {
			continue
		}
		if string(m.value) == "null" {
			continue
		}
		s, ok := jsonString(m.value)
		if !ok {
			return "", false
		}
		value = s
	}
	return value, true
}

// jsonString decodes a raw JSON string token; other tokens report false.
func jsonString(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	plain := true
	for _, c := range inner {
		if c == '\\' || c == '"' || c < 0x20 || c >= 0x80 {
			plain = false
			break
		}
	}
	if plain {
		return string(inner), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonRebuild writes an object from prefix, the members edit keeps (with the
// bytes it returns) and suffix. Kept members retain their original bytes.
func jsonRebuild(members []jsonMember, prefix []byte, edit func(jsonMember) ([]byte, bool), suffix []byte) []byte {
	size := len(prefix) + len(suffix) + 2
	for _, m := range members {
		size += len(m.span) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, '{')
	first := true
	add := func(b []byte) {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, b...)
	}
	if prefix != nil {
		add(prefix)
	}
	for _, m := range members {
		if b, keep := edit(m); keep {
			add(b)
		}
	}
	if suffix != nil {
		add(suffix)
	}
	return append(out, '}')
}

// jsonSingleLine returns data unchanged unless it holds CR or LF, which in
// valid JSON can only be insignificant whitespace; SSE needs one data line.
func jsonSingleLine(data []byte) []byte {
	if bytes.IndexByte(data, '\n') < 0 && bytes.IndexByte(data, '\r') < 0 {
		return data
	}
	var compact bytes.Buffer
	compact.Grow(len(data))
	if json.Compact(&compact, data) != nil {
		return data
	}
	return compact.Bytes()
}

// --- protected member normalization ---

// protectedRequestMembers are the top-level request members that drive
// routing, ACL, billing, quota, WebSocket transport or upstream behavior the
// proxy prices. Every reader (map decoding, first-match readers such as gjson,
// case-insensitive struct decoding) must see the single value the proxy used.
var protectedRequestMembers = []string{
	// Routing, ACL and billing identity.
	"model", "service_tier", "speed",
	// Streaming, usage reporting and the WebSocket transport envelope.
	"stream", "stream_options", "type", "background", "stream_id", "previous_response_id",
	// Content the proxy inspects for cache billing (Qwen cache_control in
	// messages/system, Kimi prompt_cache_options) or that drives input cost.
	"input", "messages", "system", "instructions", "prompt", "prompt_cache_options", "prompt_cache_key", "cache_control",
	// Output and tool budget, reasoning cost.
	"tools", "tool_choice", "max_tokens", "max_output_tokens", "max_completion_tokens", "n", "reasoning", "thinking",
	// Server-side state and attribution.
	"metadata", "conversation", "store",
}

// protectedMemberIndex maps a member to its protected canonical name, or -1.
func protectedMemberIndex(m jsonMember) int {
	if plainASCIIKey(m.key) {
		inner := m.key[1 : len(m.key)-1]
		for i, name := range protectedRequestMembers {
			if len(inner) == len(name) && strings.EqualFold(string(inner), name) {
				return i
			}
		}
		return -1
	}
	key, ok := m.decodedKey()
	if !ok {
		return -1
	}
	return protectedMemberNameIndex(key)
}

func protectedMemberNameIndex(key string) int {
	for i, name := range protectedRequestMembers {
		if strings.EqualFold(key, name) {
			return i
		}
	}
	return -1
}

// isProtectedMemberVariant reports a key that folds to a protected member
// without being its exact canonical spelling.
func isProtectedMemberVariant(key string) bool {
	i := protectedMemberNameIndex(key)
	return i >= 0 && protectedRequestMembers[i] != key
}

// memberNormalization reports one protected member that had extra members.
type memberNormalization struct {
	Member  string
	Removed int
}

// normalizeProtectedMembers leaves exactly one reading of every protected
// member: the value the proxy itself uses. Per canonical name K, among the
// top-level members whose key folds to K, the last member named exactly K is
// kept (map decoding, extractModel and ACL use it) and all others, exact
// duplicates and case variants alike, are removed. With only variants, the
// proxy saw no member, so none survives. The same rule applies to the nested
// members the proxy reads for pricing (nestedRequestSpec) inside kept
// members. Only the removed members' bytes go: other members keep their
// original bytes, order and escapes; a body needing no change is returned as
// is. Invalid JSON and non-objects pass through unchanged.
func normalizeProtectedMembers(body []byte) ([]byte, []memberNormalization) {
	if len(body) == 0 {
		return body, nil
	}
	// One validating scan records the top-level members and, through nest,
	// the nested deletions.
	nest := &nestedScan{members: make([]jsonMember, 0, 32), ranges: make([]nestedRange, 0, 32)}
	s := jsonScanner{data: body, nest: nest}
	s.space()
	if s.pos >= len(body) || body[s.pos] != '{' {
		return body, nil
	}
	open := s.pos
	s.record, s.spec = true, nestedRequestSpec
	if !s.object() {
		return body, nil
	}
	if s.space(); s.pos != len(body) {
		return body, nil
	}
	members := s.members
	drop, report := protectedMemberDrops(members)
	var edits []byteEdit
	if drop != nil {
		// The top-level entries are the members, in order.
		for i, dropped := range drop {
			if dropped {
				nest.discard(nest.ranges[i])
			}
		}
		edits = objectMemberDeletions(body, open, members, drop, edits)
	}
	edits, report = nest.live(edits, report)
	if len(edits) == 0 {
		return body, nil
	}
	return applyByteEdits(body, edits), report
}

// normalizeScannedMembers normalizes the top-level protected members only.
func normalizeScannedMembers(body []byte, members []jsonMember) ([]byte, []memberNormalization) {
	drop, report := protectedMemberDrops(members)
	if drop == nil {
		return body, nil
	}
	return applyByteEdits(body, objectMemberDeletions(body, jsonObjectOpen(body), members, drop, nil)), report
}

// jsonObjectOpen returns the offset of the opening brace of a scanned object.
func jsonObjectOpen(body []byte) int {
	open := 0
	for body[open] != '{' {
		open++
	}
	return open
}

// protectedMemberDrops marks the top-level members to remove, or returns nil
// when every protected member is already unique and exactly named.
func protectedMemberDrops(members []jsonMember) ([]bool, []memberNormalization) {
	var counts [64]int16 // matches per protected name
	var protected []int  // protected name index per member, -1 otherwise
	needed := false
	for i, m := range members {
		p := protectedMemberIndex(m)
		if p < 0 {
			continue
		}
		if protected == nil {
			protected = make([]int, len(members))
			for j := range protected {
				protected[j] = -1
			}
		}
		protected[i] = p
		counts[p]++
		if counts[p] > 1 || !m.named(protectedRequestMembers[p]) {
			needed = true
		}
	}
	if !needed {
		return nil, nil
	}
	keep := make([]int, len(protectedRequestMembers)) // member index kept per name, -1 none
	for i := range keep {
		keep[i] = -1
	}
	for i, m := range members {
		if p := protected[i]; p >= 0 && m.named(protectedRequestMembers[p]) {
			keep[p] = i
		}
	}
	removed := make([]int, len(protectedRequestMembers))
	drop := make([]bool, len(members))
	for i := range members {
		if p := protected[i]; p >= 0 && keep[p] != i {
			drop[i] = true
			removed[p]++
		}
	}
	var report []memberNormalization
	for p, n := range removed {
		if n > 0 {
			report = append(report, memberNormalization{Member: protectedRequestMembers[p], Removed: n})
		}
	}
	return drop, report
}

// --- scanner ---

// jsonMaxNestingDepth matches encoding/json's nesting limit.
const jsonMaxNestingDepth = 10000

// scanJSONObject validates data as exactly one JSON object, in one pass, and
// appends its top-level members to dst. It accepts what json.Valid accepts.
func scanJSONObject(dst []jsonMember, data []byte) ([]jsonMember, bool) {
	s := jsonScanner{data: data, members: dst}
	s.space()
	if s.pos >= len(data) || data[s.pos] != '{' {
		return dst, false
	}
	s.record = true
	if !s.object() {
		return dst, false
	}
	s.space()
	return s.members, s.pos == len(data)
}

type jsonScanner struct {
	data    []byte
	pos     int
	depth   int
	record  bool // record the members of the object being entered
	members []jsonMember

	// Nested normalization (normalizeProtectedMembers only): spec applies to
	// the next object value, arraySpec to the object elements of the next
	// array value; both are consumed by the value they precede.
	nest            *nestedScan
	spec, arraySpec *nestedSpec
}

func (s *jsonScanner) space() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScanner) value() bool {
	if s.pos >= len(s.data) {
		return false
	}
	switch c := s.data[s.pos]; {
	case c == '{':
		return s.object()
	case c == '[':
		return s.array()
	case c == '"':
		return s.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return s.number()
	case c == 't':
		return s.literal("true")
	case c == 'f':
		return s.literal("false")
	case c == 'n':
		return s.literal("null")
	}
	return false
}

func (s *jsonScanner) enter() bool {
	s.depth++
	s.pos++
	return s.depth <= jsonMaxNestingDepth
}

func (s *jsonScanner) object() bool {
	if s.nest != nil {
		return s.objectNested()
	}
	record := s.record
	s.record = false
	if !s.enter() {
		return false
	}
	s.space()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++
		s.depth--
		return true
	}
	for {
		keyStart := s.pos
		if !s.str() {
			return false
		}
		keyEnd := s.pos
		s.space()
		if s.pos >= len(s.data) || s.data[s.pos] != ':' {
			return false
		}
		s.pos++
		s.space()
		valueStart := s.pos
		if !s.value() {
			return false
		}
		if record {
			s.members = append(s.members, jsonMember{
				key: s.data[keyStart:keyEnd], value: s.data[valueStart:s.pos], span: s.data[keyStart:s.pos],
				keyStart: keyStart, valueStart: valueStart, valueEnd: s.pos,
			})
		}
		s.space()
		if s.pos >= len(s.data) {
			return false
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.space()
		case '}':
			s.pos++
			s.depth--
			return true
		default:
			return false
		}
	}
}

func (s *jsonScanner) array() bool {
	if s.nest != nil {
		return s.arrayNested()
	}
	if !s.enter() {
		return false
	}
	s.space()
	if s.pos < len(s.data) && s.data[s.pos] == ']' {
		s.pos++
		s.depth--
		return true
	}
	for {
		if !s.value() {
			return false
		}
		s.space()
		if s.pos >= len(s.data) {
			return false
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.space()
		case ']':
			s.pos++
			s.depth--
			return true
		default:
			return false
		}
	}
}

// objectNested is object with nested normalization: a spec'd object records
// its members, sets the spec of child values and normalizes its names at the
// end (nestedScan.finish). Other objects scan as usual.
func (s *jsonScanner) objectNested() bool {
	record := s.record
	s.record = false
	spec := s.spec
	s.spec, s.arraySpec = nil, nil
	open := s.pos
	if !s.enter() {
		return false
	}
	base := len(s.nest.members)
	s.space()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++
		s.depth--
		return true
	}
	for {
		keyStart := s.pos
		if !s.str() {
			return false
		}
		keyEnd := s.pos
		s.space()
		if s.pos >= len(s.data) || s.data[s.pos] != ':' {
			return false
		}
		s.pos++
		s.space()
		valueStart := s.pos
		editsStart, reportStart := len(s.nest.edits), len(s.nest.report)
		if spec != nil && len(spec.children) > 0 {
			key := jsonMember{key: s.data[keyStart:keyEnd]}
			for _, child := range spec.children {
				if key.named(child.name) {
					if child.array {
						s.arraySpec = child.spec
					} else {
						s.spec = child.spec
					}
					break
				}
			}
		}
		if !s.value() {
			return false
		}
		s.spec, s.arraySpec = nil, nil
		if record || spec != nil {
			member := jsonMember{
				key: s.data[keyStart:keyEnd], value: s.data[valueStart:s.pos], span: s.data[keyStart:s.pos],
				keyStart: keyStart, valueStart: valueStart, valueEnd: s.pos,
			}
			if record {
				s.members = append(s.members, member)
			}
			if spec != nil {
				s.nest.members = append(s.nest.members, member)
				s.nest.ranges = append(s.nest.ranges, nestedRange{
					editsStart: editsStart, editsEnd: len(s.nest.edits), reportStart: reportStart, reportEnd: len(s.nest.report)})
			}
		}
		s.space()
		if s.pos >= len(s.data) {
			return false
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.space()
		case '}':
			s.pos++
			s.depth--
			if spec != nil {
				s.nest.finish(s.data, spec, open, base, record)
			}
			return true
		default:
			return false
		}
	}
}

func (s *jsonScanner) arrayNested() bool {
	elements := s.arraySpec
	s.spec, s.arraySpec = nil, nil
	if !s.enter() {
		return false
	}
	s.space()
	if s.pos < len(s.data) && s.data[s.pos] == ']' {
		s.pos++
		s.depth--
		return true
	}
	for {
		s.spec = elements
		if !s.value() {
			return false
		}
		s.spec = nil
		s.space()
		if s.pos >= len(s.data) {
			return false
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.space()
		case ']':
			s.pos++
			s.depth--
			return true
		default:
			return false
		}
	}
}

// jsonStringSpecial marks bytes that end the fast path inside a string: the
// closing quote, an escape and the control characters JSON forbids.
var jsonStringSpecial = func() (table [256]bool) {
	for c := 0; c < 0x20; c++ {
		table[c] = true
	}
	table['"'] = true
	table['\\'] = true
	return table
}()

func (s *jsonScanner) str() bool {
	if s.pos >= len(s.data) || s.data[s.pos] != '"' {
		return false
	}
	s.pos++
	data := s.data
	for {
		for s.pos < len(data) && !jsonStringSpecial[data[s.pos]] {
			s.pos++
		}
		if s.pos >= len(data) {
			return false
		}
		switch data[s.pos] {
		case '"':
			s.pos++
			return true
		case '\\':
			s.pos++
			if s.pos >= len(data) {
				return false
			}
			switch data[s.pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.pos++
			case 'u':
				s.pos++
				for i := 0; i < 4; i++ {
					if s.pos >= len(data) || !isJSONHex(data[s.pos]) {
						return false
					}
					s.pos++
				}
			default:
				return false
			}
		default: // control character
			return false
		}
	}
}

func isJSONHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (s *jsonScanner) digits() bool {
	start := s.pos
	for s.pos < len(s.data) && s.data[s.pos] >= '0' && s.data[s.pos] <= '9' {
		s.pos++
	}
	return s.pos > start
}

func (s *jsonScanner) number() bool {
	if s.data[s.pos] == '-' {
		s.pos++
	}
	if s.pos >= len(s.data) {
		return false
	}
	switch c := s.data[s.pos]; {
	case c == '0':
		s.pos++
	case c >= '1' && c <= '9':
		s.digits()
	default:
		return false
	}
	if s.pos < len(s.data) && s.data[s.pos] == '.' {
		s.pos++
		if !s.digits() {
			return false
		}
	}
	if s.pos < len(s.data) && (s.data[s.pos] == 'e' || s.data[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.data) && (s.data[s.pos] == '+' || s.data[s.pos] == '-') {
			s.pos++
		}
		if !s.digits() {
			return false
		}
	}
	return true
}

func (s *jsonScanner) literal(word string) bool {
	if len(s.data)-s.pos < len(word) || string(s.data[s.pos:s.pos+len(word)]) != word {
		return false
	}
	s.pos += len(word)
	return true
}
