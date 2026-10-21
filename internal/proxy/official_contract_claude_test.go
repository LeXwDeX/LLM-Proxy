package proxy

import "testing"

// The fixture schema has no note field, so provenance notes live here:
// messages-iterations is a shape synthesized from the threshold compaction
// contract (https://platform.claude.com/docs/en/build-with-claude/compaction-threshold,
// checked 2026-10-21), not a verbatim doc example. Its values are this suite's
// own: top-level carries cache_read_input_tokens while the single "message"
// item omits cache counters, as the doc's items do. messages-iterations-advisor
// carries the advisor tool doc example verbatim as its usage object.
func TestOfficialContractClaude(t *testing.T) { runOfficialContracts(t, "claude") }
