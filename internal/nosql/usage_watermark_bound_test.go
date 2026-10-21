package nosql

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// The scan clock for every case below: 2026-10-21 12:30 UTC.
var boundScanTime = time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC)

const corruptCell = "{not json"

func boundEvent(id string, at time.Time) usage.Event {
	return explicitPlainEvent(id, at)
}

// seedRawThenResetWatermark writes retained raw rows, then removes the
// watermark marker so the next explicit write performs the upgrade scan.
func seedRawThenResetWatermark(t *testing.T, db *bolt.DB, events ...usage.Event) {
	t.Helper()
	store := NewUsageStore(db)
	for _, event := range events {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	resetReceiptTracking(t, db)
}

func clockedStore(db *bolt.DB) *UsageStore {
	store := NewUsageStore(db)
	store.now = func() time.Time { return boundScanTime }
	return store
}

type pendingCase struct {
	event    usage.Event
	rejected bool
}

// replayPending journals the events, replays them through a clocked store and
// checks which ones were moved to the rejected sidecar.
func replayPending(t *testing.T, store *UsageStore, cases ...pendingCase) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.journal")
	var lines []string
	for _, c := range cases {
		lines = append(lines, journalLine(t, c.event))
	}
	mustWriteFile(t, path, strings.Join(lines, "\n")+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := journalContent(t, path); got != "" {
		t.Fatalf("pending lines left in journal: %s", got)
	}
	rejected := make(map[string]string)
	for _, entry := range readRejectedSidecar(t, path) {
		rejected[entry.RecordID] = entry.Reason
	}
	for _, c := range cases {
		reason, inSidecar := rejected[c.event.RecordID]
		if inSidecar != c.rejected {
			t.Fatalf("event at %s: in sidecar=%v (reason %q), want %v", c.event.Timestamp.Format(time.RFC3339), inSidecar, reason, c.rejected)
		}
		if inSidecar && reason != usageRejectIdentityUnverified {
			t.Fatalf("event at %s rejected for %q, want %q", c.event.Timestamp.Format(time.RFC3339), reason, usageRejectIdentityUnverified)
		}
	}
}

func assertNewWriteAccepted(t *testing.T, store *UsageStore, id string) {
	t.Helper()
	if added, err := store.RecordOnce(boundEvent(id, boundScanTime.Add(time.Minute))); err != nil || !added {
		t.Fatalf("new explicit write after the scan: added=%v err=%v", added, err)
	}
}

// TestCorruptCellInFutureHourDoesNotMoveWatermark: a corrupt cell in an hour
// after every surviving raw row cannot cover pruned rows, so it only logs.
func TestCorruptCellInFutureHourDoesNotMoveWatermark(t *testing.T) {
	logs := captureDefaultLog(t)
	db := testDB(t)
	seedRawThenResetWatermark(t, db, boundEvent("55555555-5555-4555-8555-555555555501", boundScanTime.Add(-48*time.Hour)))
	futureKey := "2026-10-26T05|claude|alice|executor|anthropic"
	putAggRaw(t, db, futureKey, []byte(corruptCell))
	store := clockedStore(db)
	replayPending(t, store,
		pendingCase{event: boundEvent("55555555-5555-4555-8555-555555555502", boundScanTime.Add(-20*time.Minute))},
		pendingCase{event: boundEvent("55555555-5555-4555-8555-555555555503", boundScanTime.Add(-26*time.Hour))},
	)
	assertNewWriteAccepted(t, store, "55555555-5555-4555-8555-555555555504")
	if got := receiptWatermark(t, db); !got.IsZero() {
		t.Fatalf("future corrupt cell moved the watermark to %v", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, futureKey) {
		t.Fatalf("future corrupt cell not logged at ERROR:\n%s", out)
	}
}

// TestCorruptCellInRecentHourWithEarlierRawDoesNotMoveWatermark: the newest
// hour's own cell is corrupt while older raw history survives. The raw rows of
// that hour no longer match a decodable cell, which is caused by the corrupt
// cell itself and is not evidence of pruning.
func TestCorruptCellInRecentHourWithEarlierRawDoesNotMoveWatermark(t *testing.T) {
	db := testDB(t)
	recent := boundEvent("66666666-6666-4666-8666-666666666602", boundScanTime.Add(-70*time.Minute))
	seedRawThenResetWatermark(t, db,
		boundEvent("66666666-6666-4666-8666-666666666601", boundScanTime.Add(-72*time.Hour)),
		recent,
	)
	putAggRaw(t, db, string(aggKey(recent.Timestamp, recent.EndpointType, recent.ClientName, recent.Model, recent.Target)), []byte(corruptCell))
	putQuotaAggRaw(t, db, string(quotaAggKey(recent.Timestamp, recent.ClientName, recent.Target, recent.Model)), []byte(corruptCell))
	store := clockedStore(db)
	// Same hour, another cell: the corrupt cell itself still rejects writes
	// (bumpAgg never overwrites ledger bytes), which is a separate fail-closed
	// rule; this case is only about the watermark.
	sameHour := boundEvent("66666666-6666-4666-8666-666666666603", recent.Timestamp.Add(5*time.Minute))
	sameHour.Model = "advisor"
	replayPending(t, store,
		pendingCase{event: sameHour},
		pendingCase{event: boundEvent("66666666-6666-4666-8666-666666666604", boundScanTime.Add(-10*time.Minute))},
	)
	assertNewWriteAccepted(t, store, "66666666-6666-4666-8666-666666666605")
	if got := receiptWatermark(t, db); !got.IsZero() {
		t.Fatalf("recent corrupt cell moved the watermark to %v", got)
	}
}

// TestCorruptCellInsidePrunedRangeStillMovesWatermark: corrupt evidence in an
// hour that retention may have cleaned keeps the conservative boundary.
func TestCorruptCellInsidePrunedRangeStillMovesWatermark(t *testing.T) {
	survivor := boundEvent("77777777-7777-4777-8777-777777777701", boundScanTime.Add(-48*time.Hour))
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, db *bolt.DB)
		bad   time.Time
	}{
		{
			name:  "before earliest surviving raw",
			setup: func(t *testing.T, db *bolt.DB) {},
			bad:   boundScanTime.Add(-240 * time.Hour),
		},
		{
			name: "same hour as earliest surviving raw",
			setup: func(t *testing.T, db *bolt.DB) {
			},
			bad: survivor.Timestamp,
		},
		{
			name: "covered by a later receipt",
			setup: func(t *testing.T, db *bolt.DB) {
				// A receipt proves retention cleaned rows up to its hour; the
				// survivor is an out-of-order insert below that boundary.
				putUsageReceipt(t, db, usageKey(boundScanTime.Add(-30*time.Hour), "77777777-7777-4777-8777-7777777777ff"), usageReceipt{Version: usageReceiptVersion, Digest: "d"})
			},
			bad: boundScanTime.Add(-40 * time.Hour),
		},
		{
			name: "covered by a later pruned hour",
			setup: func(t *testing.T, db *bolt.DB) {
				// Rollups count a row whose raw detail is gone (old prune).
				pruned := boundEvent("77777777-7777-4777-8777-777777777702", boundScanTime.Add(-30*time.Hour))
				if err := NewUsageStore(db).Record(pruned); err != nil {
					t.Fatal(err)
				}
				deleteUsageRawAndIndex(t, db, pruned)
				resetReceiptTracking(t, db)
			},
			bad: boundScanTime.Add(-40 * time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			seedRawThenResetWatermark(t, db, survivor)
			tc.setup(t, db)
			badHour := tc.bad.Truncate(time.Hour)
			putAggRaw(t, db, badHour.Format(hourLayout)+"|claude|bob|executor|anthropic", []byte(corruptCell))
			store := clockedStore(db)
			replayPending(t, store,
				pendingCase{event: boundEvent("77777777-7777-4777-8777-777777777703", badHour.Add(10*time.Minute)), rejected: true},
				pendingCase{event: boundEvent("77777777-7777-4777-8777-777777777704", boundScanTime.Add(-10*time.Minute))},
			)
			assertNewWriteAccepted(t, store, "77777777-7777-4777-8777-777777777705")
			if got := receiptWatermark(t, db); got.Before(badHour.Add(time.Hour)) {
				t.Fatalf("watermark=%v, want at least %v", got, badHour.Add(time.Hour))
			}
		})
	}
}

// TestCorruptCellWithoutRawIsConservativeButCapped: with no surviving raw the
// corrupt hour may have been cleaned, so it stays untrusted, but the watermark
// never passes the scan hour and current traffic keeps flowing.
func TestCorruptCellWithoutRawIsConservativeButCapped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bad       time.Time
		watermark time.Time
	}{
		{"past hour", boundScanTime.Add(-72 * time.Hour), boundScanTime.Add(-72 * time.Hour).Truncate(time.Hour).Add(time.Hour)},
		{"future hour", boundScanTime.Add(120 * time.Hour), boundScanTime.Truncate(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			db := testDB(t)
			putAggRaw(t, db, tc.bad.Truncate(time.Hour).Format(hourLayout)+"|claude|bob|executor|anthropic", []byte(corruptCell))
			store := clockedStore(db)
			replayPending(t, store,
				pendingCase{event: boundEvent("88888888-8888-4888-8888-888888888801", boundScanTime.Add(-20*time.Minute))},
				pendingCase{event: boundEvent("88888888-8888-4888-8888-888888888802", boundScanTime.Add(-96*time.Hour)), rejected: true},
			)
			assertNewWriteAccepted(t, store, "88888888-8888-4888-8888-888888888803")
			if got := receiptWatermark(t, db); !got.Equal(tc.watermark) {
				t.Fatalf("watermark=%v, want %v", got, tc.watermark)
			}
			if tc.name == "future hour" && !strings.Contains(logs.String(), "capped") {
				t.Fatalf("capped watermark not logged:\n%s", logs.String())
			}
		})
	}
}

// TestWatermarkNeverExceedsScanHour applies the cap to decodable evidence too:
// a legacy four-segment key in a future hour cannot block current writes.
func TestWatermarkNeverExceedsScanHour(t *testing.T) {
	db := testDB(t)
	future := boundScanTime.Add(48 * time.Hour).Truncate(time.Hour)
	putAggRaw(t, db, future.Format(hourLayout)+"|claude|bob|executor", []byte(`{"record_count":1}`))
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got, want := receiptWatermark(t, db), boundScanTime.Truncate(time.Hour); !got.Equal(want) {
		t.Fatalf("watermark=%v, want capped %v", got, want)
	}
	assertNewWriteAccepted(t, store, "99999999-9999-4999-8999-999999999901")
}
