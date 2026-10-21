//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// These are pinned data-contract test targets, NOT rollback targets. No older
// binary can be rolled back to in place after #32: b36b544 replays journals,
// reads raw rows and prices bundles, but its aggregate readers (Count,
// Aggregate, SumByClientRange) cannot parse v2 hourly cells and silently return
// zero for them without an error; v1.0.1 additionally misprices bundles. A
// rollback can only restore the complete, consistent pre-upgrade backup set
// (database, recovery journal and its sidecars, configuration, migration
// snapshots) and discard everything the new instance wrote, after confirming
// that data may be discarded. b36b544 still contains other known bugs.
const (
	bundleCompatibleSHA   = "b36b5445853c33ee0b804de46d42f6d8a1b152fa"
	bundleIncompatibleSHA = "0e7ebfd2a987356d715ac6fdc8418f215e8907fb"
	bundleProbePath       = "test/integration/testdata/usage_bundle_probe"
)

type bundleProbeReport struct {
	Events  []usage.Event
	Rollups map[string][]nosql.AggCell
	Prices  map[string]struct {
		Cost              float64
		Requests          int64
		InputTokens       int64
		EventUnits        int
		IncompleteCosts   int64
		Error             string
		HistoryIncomplete bool
	}
	Readers bundleReaderReport
}

// bundleReaderReport mirrors the probe's aggregate reader results.
type bundleReaderReport struct {
	CountTotal       int64
	CountSuccess     int64
	CountError       string
	AggregateRequest int64
	AggregateInput   int64
	AggregateError   string
	SumInput         int64
	SumError         string
}

func TestUsageBundleRollbackContract(t *testing.T) {
	root := strings.TrimSpace(string(bundleCommand(t, "", nil, "git", "rev-parse", "--show-toplevel")))
	current := filepath.Join(t.TempDir(), "probe-current")
	// Build in place, not from git archive HEAD: uncommitted implementation changes
	// are part of the contract under test too.
	bundleCommand(t, root, nil, "go", "build", "-mod=readonly", "-o", current, "./"+bundleProbePath)
	compatible := buildHistoricalBundleProbe(t, root, bundleCompatibleSHA)
	incompatible := buildHistoricalBundleProbe(t, root, bundleIncompatibleSHA)
	t.Logf("current=working tree (HEAD %s); compatible=%s; incompatible=%s",
		strings.TrimSpace(string(bundleCommand(t, root, nil, "git", "rev-parse", "HEAD"))), bundleCompatibleSHA, bundleIncompatibleSHA)

	for _, scenario := range []struct {
		name            string
		journal         bool
		committed       bool
		legacy          bool
		freeRoot        bool
		cost            float64
		appendCost      float64
		legacyUsageRows int64
	}{
		{name: "database", committed: true, cost: 12, appendCost: 12},
		{name: "journal_only", journal: true, cost: 12, appendCost: 12},
		{name: "committed_uncompacted_journal", journal: true, committed: true, cost: 12, appendCost: 12},
		{name: "nonbillable_root_mixed_legacy", journal: true, committed: true, legacy: true, freeRoot: true, cost: 12, appendCost: 10, legacyUsageRows: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			event := rollbackBundle(scenario.freeRoot)
			// expected holds every stored row; oldVisible only the rows an old
			// binary wrote in the historical aggregate key format, which is
			// all b36b544's aggregate readers can see.
			var expected, oldVisible []usage.Event
			if scenario.legacy {
				legacy := event
				legacy.RecordID = "00000000-0000-4000-8000-000000000002"
				legacy.Components, legacy.NonBillable = nil, false
				// Real v1.0.1 creates the legacy cells without record_count.
				runBundleProbe(t, incompatible, dir, "record", legacy)
				expected = append(expected, legacy)
				oldVisible = append(oldVisible, legacy)
			}
			if scenario.committed {
				runBundleProbe(t, current, dir, "record", event)
			}
			expected = append(expected, usage.ExpandEvent(event)...)
			if scenario.journal {
				runBundleProbe(t, current, dir, "journal", event)
				path := filepath.Join(dir, "usage.journal")
				pending, err := os.ReadFile(path)
				if err != nil || len(bytes.TrimSpace(pending)) == 0 {
					t.Fatalf("missing durable journal: %v", err)
				}
				if !scenario.committed {
					assertBundleReport(t, inspectBundle(t, current, dir), nil, 0, scenario.legacyUsageRows, nil)
					// The compatible binary replays the journal below and
					// writes historical-format cells for the bundle.
					oldVisible = append(oldVisible, usage.ExpandEvent(event)...)
				}
				for attempt := 0; attempt < 2; attempt++ {
					if attempt > 0 {
						// Reintroduce the same durable bytes: the second replay must
						// test deduplication, not merely an already-empty journal.
						if err := os.WriteFile(path, pending, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					var recovered []string
					if err := json.Unmarshal(runBundleProbe(t, compatible, dir, "replay"), &recovered); err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(recovered, []string{"rollback-contract"}) {
						t.Fatalf("recovered clients=%v", recovered)
					}
					assertBundleJournalEmpty(t, dir)
					assertBundleReport(t, inspectBundle(t, compatible, dir), expected, scenario.cost, scenario.legacyUsageRows, oldVisible)
					assertBundleReport(t, inspectBundle(t, current, dir), expected, scenario.cost, scenario.legacyUsageRows, expected)
				}
			} else {
				assertBundleReport(t, inspectBundle(t, compatible, dir), expected, scenario.cost, scenario.legacyUsageRows, oldVisible)
				assertBundleReport(t, inspectBundle(t, current, dir), expected, scenario.cost, scenario.legacyUsageRows, expected)
			}

			// Overwrite the SAME hour/model cells using the fixed older source.
			continued := event
			continued.RecordID = "00000000-0000-4000-8000-000000000003"
			continued.Timestamp = continued.Timestamp.Add(time.Minute)
			runBundleProbe(t, compatible, dir, "record", continued)
			expected = append(expected, usage.ExpandEvent(continued)...)
			oldVisible = append(oldVisible, usage.ExpandEvent(continued)...)
			// The fixed older source rewrites the old cell with record_count,
			// so every expanded row is provable again at this step.
			assertBundleReport(t, inspectBundle(t, compatible, dir), expected, scenario.cost+scenario.appendCost, 0, oldVisible)
			assertBundleReport(t, inspectBundle(t, current, dir), expected, scenario.cost+scenario.appendCost, 0, expected)
			assertBundleJournalEmpty(t, dir)
		})
	}

	// Counterexample for #13/#35: after #32 b36b544 is not a rollback target
	// either. Its raw reads and pricing still see the bundle, but every
	// aggregate reader silently reports zero for v2 cells (nil error).
	t.Run("b36b544_cannot_read_v2_rollups", func(t *testing.T) {
		dir := t.TempDir()
		event := rollbackBundle(false)
		runBundleProbe(t, current, dir, "record", event)
		rows := usage.ExpandEvent(event)
		assertBundleReport(t, inspectBundle(t, current, dir), rows, 12, 0, rows)
		old := inspectBundle(t, compatible, dir)
		assertBundleReport(t, old, rows, 12, 0, nil)
		if old.Readers != (bundleReaderReport{}) {
			t.Fatalf("b36b544 aggregate readers saw v2 cells: %+v", old.Readers)
		}
		var stored int64
		for _, cell := range old.Rollups[nosql.BucketUsageAggHourly] {
			stored += cell.Requests
		}
		if stored != 1 {
			t.Fatalf("v2 rollup bytes missing from the database: requests=%d", stored)
		}
		t.Logf("%s: raw rows=%d, stored rollup requests=%d, aggregate readers=%+v", bundleCompatibleSHA, len(old.Events), stored, old.Readers)
	})

	t.Run("v1_0_1_is_not_a_rollback_target", func(t *testing.T) {
		dir := t.TempDir()
		event := rollbackBundle(false)
		runBundleProbe(t, current, dir, "record", event)
		assertBundleReport(t, inspectBundle(t, current, dir), usage.ExpandEvent(event), 12, 0, usage.ExpandEvent(event))
		old := inspectBundle(t, incompatible, dir)
		if old.Readers != (bundleReaderReport{}) {
			t.Fatalf("v1.0.1 aggregate readers saw v2 cells: %+v", old.Readers)
		}
		stable, exact := old.Prices["stable"], old.Prices["exact"]
		if stable.Error != "" || stable.Cost != 27 || stable.Requests != 1 || stable.InputTokens != 6_000_000 {
			t.Fatalf("expected v1.0.1 overcharge (27, not 12), got %+v", stable)
		}
		if !exact.HistoryIncomplete || !strings.Contains(exact.Error, "raw request count 2 differs from rollup count 0") {
			t.Fatalf("expected typed ErrUsageHistoryIncomplete from record parity, got %+v", exact)
		}
		t.Logf("%s: stable cost=%g requests=%d tokens=%d; exact=%s", bundleIncompatibleSHA, stable.Cost, stable.Requests, stable.InputTokens, exact.Error)
		assertBundleReport(t, inspectBundle(t, current, dir), usage.ExpandEvent(event), 12, 0, usage.ExpandEvent(event))
	})
}

func rollbackBundle(freeRoot bool) usage.Event {
	return usage.Event{
		RecordID:  "00000000-0000-4000-8000-000000000001",
		Timestamp: time.Date(2026, 10, 14, 9, 15, 0, 0, time.UTC), ClientName: "rollback-contract",
		Target: "anthropic", EndpointType: "claude", Model: "executor", Path: "/v1/messages", StatusCode: 200,
		InputTokens: 1_000_000, ObservedFields: usage.UsageFieldInput, NonBillable: freeRoot,
		PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: 1_000_000, TotalInputKnown: true, TotalInputValid: true, ActualSpeed: "fast"},
		Components: []usage.Component{
			{Model: "advisor", IterationType: "advisor", IterationIndex: 0, InputTokens: 2_000_000, ObservedFields: usage.UsageFieldInput,
				PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: 2_000_000, TotalInputKnown: true, TotalInputValid: true}},
			{Model: "advisor", IterationType: "advisor", IterationIndex: 1, InputTokens: 3_000_000, ObservedFields: usage.UsageFieldInput, NonBillable: true},
		},
	}
}

// legacyUsageRows counts rows written by binaries that predate record_count.
// Their cells carry tokens and request totals but no row count, so the usage
// aggregate ledger alone cannot prove those rows. readable lists the rows the
// inspecting binary's aggregate readers can parse: every row for the current
// binary, only historical-format cells for b36b544.
func assertBundleReport(t *testing.T, got bundleProbeReport, want []usage.Event, cost float64, legacyUsageRows int64, readable []usage.Event) {
	t.Helper()
	assertBundleReaders(t, got.Readers, readable)
	want = append([]usage.Event{}, want...)
	byID := func(a, b usage.Event) int { return strings.Compare(a.RecordID, b.RecordID) }
	slices.SortFunc(want, byID)
	slices.SortFunc(got.Events, byID)
	if !reflect.DeepEqual(got.Events, want) {
		t.Fatalf("stored bundle/context changed:\ngot  %+v\nwant %+v", got.Events, want)
	}
	var requests, tokens, freeTokens int64
	for _, event := range want {
		if !event.Supplementary {
			requests++
		}
		tokens += event.InputTokens
		if event.NonBillable {
			freeTokens += event.InputTokens
		}
	}
	for _, mode := range []string{"stable", "exact", "conditional"} {
		price, ok := got.Prices[mode]
		if !ok || price.Error != "" || price.HistoryIncomplete || price.Cost != cost || price.Requests != requests || price.InputTokens != tokens || price.IncompleteCosts != 0 {
			t.Fatalf("%s: got %+v; want cost=%g requests=%d tokens=%d", mode, price, cost, requests, tokens)
		}
		wantEventUnits := len(want)
		if mode == "stable" {
			wantEventUnits = 0
		}
		if price.EventUnits != wantEventUnits {
			t.Fatalf("%s took wrong pricing path: event units=%d, want %d", mode, price.EventUnits, wantEventUnits)
		}
	}
	for _, name := range []string{nosql.BucketUsageAggHourly, nosql.BucketQuotaUsageAggHourly} {
		var records, rollupRequests, rollupTokens, rollupFree int64
		for _, cell := range got.Rollups[name] {
			records += cell.RecordCount
			rollupRequests += cell.Requests
			rollupTokens += cell.InputTokens
			if cell.NonBillable != nil {
				rollupFree += cell.NonBillable.InputTokens
			}
		}
		wantRecords := int64(len(want))
		if name == nosql.BucketUsageAggHourly {
			wantRecords -= legacyUsageRows
		}
		if records != wantRecords || rollupRequests != requests || rollupTokens != tokens || rollupFree != freeTokens {
			t.Fatalf("%s lost record_count/non_billable: records=%d want=%d requests=%d tokens=%d free=%d", name, records, wantRecords, rollupRequests, rollupTokens, rollupFree)
		}
	}
	t.Logf("cost=%g records=%d requests=%d observed_tokens=%d non_billable_tokens=%d (stable/exact/conditional)", cost, len(want), requests, tokens, freeTokens)
}

// assertBundleReaders checks Count, Aggregate and SumByClientRange totals
// against the rows the binary can read. All three must succeed, so a binary
// that cannot parse a cell under-reports silently instead of failing.
func assertBundleReaders(t *testing.T, got bundleReaderReport, readable []usage.Event) {
	t.Helper()
	var requests, tokens int64
	for _, event := range readable {
		if !event.Supplementary {
			requests++
		}
		tokens += event.InputTokens
	}
	want := bundleReaderReport{
		CountTotal: requests, CountSuccess: requests,
		AggregateRequest: requests, AggregateInput: tokens,
		SumInput: tokens,
	}
	if got != want {
		t.Fatalf("aggregate readers=%+v, want %+v", got, want)
	}
}

func assertBundleJournalEmpty(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"usage.journal", "usage.journal.corrupt", "usage.journal.rejected"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(data)) != 0 {
			t.Fatalf("legal journal rows remain pending or quarantined in %s: %s", name, data)
		}
	}
}

func inspectBundle(t *testing.T, binary, dir string) bundleProbeReport {
	t.Helper()
	var result bundleProbeReport
	if err := json.Unmarshal(runBundleProbe(t, binary, dir, "inspect"), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func runBundleProbe(t *testing.T, binary, dir, action string, events ...usage.Event) []byte {
	t.Helper()
	input, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return bundleCommand(t, "", input, binary, action, dir)
}

func buildHistoricalBundleProbe(t *testing.T, root, sha string) string {
	t.Helper()
	// Missing fixed objects are a failed gate, never a skipped compatibility test.
	bundleCommand(t, root, nil, "git", "cat-file", "-e", sha+"^{commit}")
	dir := t.TempDir()
	archive := filepath.Join(t.TempDir(), "source.tar")
	bundleCommand(t, root, nil, "git", "archive", "--format=tar", "--output="+archive, sha)
	bundleCommand(t, root, nil, "tar", "-xf", archive, "-C", dir)
	driver, err := os.ReadFile(filepath.Join(root, bundleProbePath, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	probeDir := filepath.Join(dir, bundleProbePath)
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "main.go"), driver, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "probe-"+sha)
	bundleCommand(t, dir, nil, "go", "build", "-mod=readonly", "-o", binary, "./"+bundleProbePath)
	return binary
}

func bundleCommand(t *testing.T, dir string, input []byte, program string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s\n%s", program, args, dir, err, out, stderr.String())
	}
	return out
}
