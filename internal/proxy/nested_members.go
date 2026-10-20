// nested_members.go — 代理会读取的嵌套计价成员（stream_options、prompt_cache_options、cache_control）的规范化。
package proxy

import "bytes"

// nestedSpec is one JSON object position whose members the proxy reads for
// pricing: names are normalized like top-level protected members (the last
// exact member is kept, other exact duplicates and case variants removed,
// with only variants none kept); children lead to deeper objects, through an
// object member or through every object element of an array member.
type nestedSpec struct {
	path     string // report prefix, e.g. "messages[].content[]"
	names    []string
	children []nestedChild
}

type nestedChild struct {
	name  string
	array bool
	spec  *nestedSpec
}

// cacheControlSpec covers a cache_control object (Qwen and Anthropic cache
// marks: type, and the Anthropic cache write ttl).
func cacheControlSpec(path string) *nestedSpec {
	return &nestedSpec{path: path, names: []string{"type", "ttl"}}
}

// cacheBlockSpec covers an object that may carry cache_control: a message
// content block, a system block or a tool.
func cacheBlockSpec(path string) *nestedSpec {
	return &nestedSpec{path: path, names: []string{"cache_control"},
		children: []nestedChild{{name: "cache_control", spec: cacheControlSpec(path + ".cache_control")}}}
}

// nestedRequestSpec mirrors what the proxy reads below the top level:
// stream_options.include_usage (stream usage policy), the Kimi
// prompt_cache_options.ttl, and cache_control where hasEphemeralCacheControl
// looks (top level, system[*], messages[*], messages[*].content[*]) plus
// tools[*]. A message's content is normalized too, since the cache marks
// read depend on which content survives. Top-level names (cache_control
// included) belong to protectedRequestMembers.
var nestedRequestSpec = &nestedSpec{children: []nestedChild{
	{name: "stream_options", spec: &nestedSpec{path: "stream_options", names: []string{"include_usage", "include_obfuscation"}}},
	{name: "prompt_cache_options", spec: &nestedSpec{path: "prompt_cache_options", names: []string{"ttl"}}},
	{name: "cache_control", spec: cacheControlSpec("cache_control")},
	{name: "messages", array: true, spec: &nestedSpec{path: "messages[]", names: []string{"cache_control", "content"},
		children: []nestedChild{
			{name: "cache_control", spec: cacheControlSpec("messages[].cache_control")},
			{name: "content", array: true, spec: cacheBlockSpec("messages[].content[]")},
		}}},
	{name: "system", array: true, spec: cacheBlockSpec("system[]")},
	{name: "tools", array: true, spec: cacheBlockSpec("tools[]")},
}}

// nestedScan collects, during the one validating scan of a request body, the
// deletions for nested members (jsonScanner.nest). Edits and report entries
// found inside a member that is itself dropped later are discarded.
type nestedScan struct {
	edits  []byteEdit            // start < 0: discarded
	report []memberNormalization // Removed 0: discarded
	// members of the open spec'd objects (a stack), and per member the edits
	// and report entries its value produced.
	members []jsonMember
	ranges  []nestedRange
}

type nestedRange struct {
	editsStart, editsEnd   int
	reportStart, reportEnd int
}

func (n *nestedScan) discard(r nestedRange) {
	for i := r.editsStart; i < r.editsEnd; i++ {
		n.edits[i].start = -1
	}
	for i := r.reportStart; i < r.reportEnd; i++ {
		n.report[i].Removed = 0
	}
}

// finish normalizes spec.names within the object at open whose members are
// members[base:], then pops them unless keep (the top level keeps them for
// the protected-member pass).
func (n *nestedScan) finish(data []byte, spec *nestedSpec, open, base int, keep bool) {
	if members := n.members[base:]; len(spec.names) > 0 && len(members) > 0 {
		if drop, removed := foldedMemberDrops(members, spec.names); drop != nil {
			for i, dropped := range drop {
				if dropped {
					n.discard(n.ranges[base+i])
				}
			}
			n.edits = objectMemberDeletions(data, open, members, drop, n.edits)
			for k, count := range removed {
				if count > 0 {
					n.report = append(n.report, memberNormalization{Member: spec.path + "." + spec.names[k], Removed: count})
				}
			}
		}
	}
	if !keep {
		n.members, n.ranges = n.members[:base], n.ranges[:base]
	}
}

// live returns the kept edits and the report aggregated per member path in
// order of first appearance.
func (n *nestedScan) live(edits []byteEdit, report []memberNormalization) ([]byteEdit, []memberNormalization) {
	for _, edit := range n.edits {
		if edit.start >= 0 {
			edits = append(edits, edit)
		}
	}
	first := len(report)
	for _, entry := range n.report {
		if entry.Removed == 0 {
			continue
		}
		merged := false
		for i := first; i < len(report); i++ {
			if report[i].Member == entry.Member {
				report[i].Removed += entry.Removed
				merged = true
				break
			}
		}
		if !merged {
			report = append(report, entry)
		}
	}
	return edits, report
}

// foldedMemberDrops applies the protected-member rule to names within one
// object: per name, the last member named exactly is kept and every other
// member whose key folds to it is dropped. drop is nil when nothing drops.
func foldedMemberDrops(members []jsonMember, names []string) (drop []bool, removed []int) {
	// First pass without allocation: most objects need nothing.
	var counts [4]int
	keep := [4]int{-1, -1, -1, -1}
	needed := false
	for i, m := range members {
		if k := foldedNameIndex(m, names); k >= 0 {
			counts[k]++
			if m.named(names[k]) {
				keep[k] = i
			} else {
				needed = true
			}
			if counts[k] > 1 {
				needed = true
			}
		}
	}
	if !needed {
		return nil, nil
	}
	drop = make([]bool, len(members))
	removed = make([]int, len(names))
	for i, m := range members {
		if k := foldedNameIndex(m, names); k >= 0 && keep[k] != i {
			drop[i] = true
			removed[k]++
		}
	}
	return drop, removed
}

func foldedNameIndex(m jsonMember, names []string) int {
	for k, name := range names {
		if m.folds(name) {
			return k
		}
	}
	return -1
}

// objectMemberDeletions appends the byte deletions that remove the dropped
// members of the object at open: each dropped member goes with the separator
// before it, and when the first members are dropped the comma before the first
// kept member goes too. Kept members and all other bytes stay as written.
func objectMemberDeletions(body []byte, open int, members []jsonMember, drop []bool, edits []byteEdit) []byteEdit {
	previousEnd := open + 1
	leading := true
	for i, m := range members {
		if drop[i] {
			edits = append(edits, byteEdit{start: previousEnd, end: m.valueEnd})
		} else {
			if leading && i > 0 {
				comma := bytes.IndexByte(body[previousEnd:m.keyStart], ',')
				edits = append(edits, byteEdit{start: previousEnd, end: previousEnd + comma + 1})
			}
			leading = false
		}
		previousEnd = m.valueEnd
	}
	return edits
}
