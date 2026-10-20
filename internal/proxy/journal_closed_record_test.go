package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// A usage event recorded after the shutdown drain closed the journal never
// panics and is never silently lost: the journal falls back to a synchronous
// commit (bbolt, else a durable journal append replayed on the next start);
// only when both fail does Record return an error, which the proxy counts as
// a usage record failure and logs.
func TestUsageRecordAfterJournalClose(t *testing.T) {
	for _, tc := range []struct {
		name               string
		closeDB, blockFile bool
	}{
		{name: "db_open"},
		{name: "db_closed_journal_append", closeDB: true},
		{name: "db_closed_journal_unwritable", closeDB: true, blockFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersistenceFixture(t, false)
			dbPath := f.db.Path()
			journalPath := dbPath + ".usage-journal.jsonl"
			if err := f.journal.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.closeDB {
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.blockFile {
				if err := os.RemoveAll(journalPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(journalPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.streamDoneToEOF(http.DefaultClient); err != nil {
				t.Fatalf("request after journal close: %v", err)
			}
			failures := f.service.MetricsSnapshot().TotalUsageRecordFailures
			switch {
			case tc.blockFile:
				if failures != 1 {
					t.Fatalf("unpersisted usage must count as a record failure: %d", failures)
				}
				return
			case failures != 0:
				t.Fatalf("durable fallback counted as a failure: %d", failures)
			}
			db := f.db
			if tc.closeDB {
				reopened, err := nosql.OpenDB(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				store := nosql.NewUsageStore(reopened)
				journal, err := nosql.NewUsageJournal(store, filepath.Clean(journalPath), newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := journal.Replay(); err != nil {
					t.Fatal(err)
				}
				db = reopened
			}
			events, err := nosql.NewUsageStore(db).List(usage.Filter{ClientName: "tester", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].InputTokens != 7 || events[0].OutputTokens != 3 {
				t.Fatalf("usage after journal close lost: %+v", events)
			}
		})
	}
}
