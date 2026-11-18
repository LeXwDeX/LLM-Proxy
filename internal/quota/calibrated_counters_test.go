package quota

// Issue #211: a client with no plan group cannot accumulate usage, so
// calibration must not read the authoritative usage history for it.

import (
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestCalibratedCountersSkipsReadWithoutPlanGroups(t *testing.T) {
	now := time.Now().UTC()
	levels := map[string]config.JobLevel{"l1": {Name: "L1"}}
	readErr := errors.New("read must not run")
	calls := 0
	read := func(usage.Filter) ([]nosql.CostUnit, error) {
		calls++
		return nil, readErr
	}

	for _, plan := range [][]config.QuotaGroup{
		nil,
		{},
		{{Name: "  "}},
	} {
		calls = 0
		qc := quotaConfig{client: &config.Client{Name: "new", JobLevel: "L1"}, plan: plan, levels: levels}
		cc, err := calibratedCounters("new", qc, now, read)
		if err != nil {
			t.Fatalf("plan %#v: unexpected error %v", plan, err)
		}
		if calls != 0 {
			t.Fatalf("plan %#v: authoritative read ran %d times, want 0", plan, calls)
		}
		if cc == nil || len(cc.groups) != 0 {
			t.Fatalf("plan %#v: counters %+v, want empty group set", plan, cc)
		}
		if cc.jobLevelMissing {
			t.Fatalf("plan %#v: bound job level reported missing", plan)
		}
	}

	// The client-level job-level gate is independent of the skipped read.
	qc := quotaConfig{client: &config.Client{Name: "new"}, levels: levels}
	cc, err := calibratedCounters("new", qc, now, read)
	if err != nil {
		t.Fatalf("missing job level: %v", err)
	}
	if !cc.jobLevelMissing {
		t.Fatal("client without a job level must stay rejected after skipping the read")
	}

	// A plan group needs the authoritative read; its error is still propagated.
	calls = 0
	qc = quotaConfig{
		client:  &config.Client{Name: "new", JobLevel: "L1"},
		plan:    []config.QuotaGroup{{Name: "plan"}},
		catalog: map[string]config.ModelGroup{"plan": {Name: "plan", CostCoefficient: 1}},
		levels:  levels,
	}
	if _, err := calibratedCounters("new", qc, now, read); !errors.Is(err, readErr) {
		t.Fatalf("plan read error = %v, want %v", err, readErr)
	}
	if calls != 1 {
		t.Fatalf("plan read ran %d times, want 1", calls)
	}
}