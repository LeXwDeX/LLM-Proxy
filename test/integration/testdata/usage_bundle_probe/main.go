// This driver deliberately uses APIs shared by all pinned versions. Each build
// links that version's real storage, journal and pricing implementations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// readerResult reports the aggregate readers an operator sees (overview totals,
// usage reports and the legacy quota fallback) as each pinned binary sees them.
type readerResult struct {
	CountTotal       int64
	CountSuccess     int64
	CountError       string
	AggregateRequest int64
	AggregateInput   int64
	AggregateError   string
	SumInput         int64
	SumError         string
}

type priceResult struct {
	Cost              float64
	Requests          int64
	InputTokens       int64
	EventUnits        int
	IncompleteCosts   int64
	Error             string
	HistoryIncomplete bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 3 {
		return errors.New("usage: usage_bundle_probe record|journal|replay|inspect DATA_DIR")
	}
	action, dir := os.Args[1], os.Args[2]
	db, err := nosql.OpenDB(filepath.Join(dir, "usage.db"))
	if err != nil {
		return err
	}
	if action != "journal" {
		defer db.Close()
	}
	store := nosql.NewUsageStore(db)
	journal, err := nosql.NewUsageJournal(store, filepath.Join(dir, "usage.journal"), nil)
	if err != nil {
		return err
	}
	switch action {
	case "record", "journal":
		var recorder usage.Recorder = store
		if action == "journal" {
			// A real closed DB forces the production synchronous recovery path.
			if err := db.Close(); err != nil {
				return err
			}
			recorder = journal
		}
		var events []usage.Event
		if err := json.NewDecoder(os.Stdin).Decode(&events); err != nil {
			return err
		}
		for _, event := range events {
			if err := recorder.Record(event); err != nil {
				return err
			}
		}
		return nil
	case "replay":
		recovered, err := journal.Replay()
		if err != nil {
			return err
		}
		if err := journal.DrainClient("rollback-contract"); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(recovered)
	case "inspect":
		return inspect(db, store)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func inspect(db *bolt.DB, store *nosql.UsageStore) error {
	from := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	filter := usage.Filter{From: &from, To: &to, ClientName: "rollback-contract"}
	events, err := store.List(filter)
	if err != nil {
		return err
	}
	rollups := make(map[string][]json.RawMessage)
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{nosql.BucketUsageAggHourly, nosql.BucketQuotaUsageAggHourly} {
			if err := tx.Bucket([]byte(name)).ForEach(func(_, value []byte) error {
				rollups[name] = append(rollups[name], append(json.RawMessage(nil), value...))
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	prices := make(map[string]priceResult)
	for _, mode := range []string{"stable", "exact", "conditional"} {
		templates := []pricing.PricingTemplate{
			{ID: "root", Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			{ID: "child", Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
		}
		if mode == "exact" {
			// A non-hour-aligned effective time forces raw/rollup parity checks.
			for i := range templates {
				templates[i].Start = from.Add(time.Minute)
			}
		}
		if mode == "conditional" {
			threshold := int64(1_500_000)
			templates[0].Conditions = []pricing.PriceCondition{{ID: "long-root", MinInputTokens: &threshold, Rates: usage.CostRates{InputPer1MTokens: 20}}}
			templates[1].BaseRates.InputPer1MTokens = 50
			templates[1].Conditions = []pricing.PriceCondition{{ID: "long-child", MinInputTokens: &threshold, Rates: usage.CostRates{InputPer1MTokens: 5}}}
		}
		resolver := pricing.NewResolver(templates, nil)
		units, err := store.ReadCostUnits(context.Background(), filter, resolver, true)
		result := priceResult{}
		if err != nil {
			result.Error = err.Error()
			result.HistoryIncomplete = errors.Is(err, nosql.ErrUsageHistoryIncomplete)
		}
		for _, unit := range units {
			result.Cost += unit.CostUSD
			result.Requests += unit.Requests
			result.InputTokens += unit.InputTokens
			result.IncompleteCosts += unit.IncompleteCostCount
			if unit.EventPriced {
				result.EventUnits++
			}
		}
		prices[mode] = result
	}
	readers := readerResult{}
	if total, success, err := store.Count(from, to); err != nil {
		readers.CountError = err.Error()
	} else {
		readers.CountTotal, readers.CountSuccess = total, success
	}
	if result, err := store.Aggregate(filter, "hour", nil); err != nil {
		readers.AggregateError = err.Error()
	} else {
		readers.AggregateRequest, readers.AggregateInput = result.Totals.Requests, result.Totals.InputTokens
	}
	if totals, err := store.SumByClientRange(filter.ClientName, from, to); err != nil {
		readers.SumError = err.Error()
	} else {
		for _, total := range totals {
			readers.SumInput += total.InputTokens
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Events  []usage.Event
		Rollups map[string][]json.RawMessage
		Prices  map[string]priceResult
		Readers readerResult
	}{events, rollups, prices, readers})
}
