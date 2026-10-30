package nosql

import (
	"math/rand"
	"testing"
	"time"
)

// TestIntegrityReadVerdictDeterministic pins that an unchanged database gives
// the same verdict on every read. The differential harness compares one Fast
// read with one Full read, so it only catches a nondeterministic first error
// when the two random choices happen to differ; this test repeats the same
// read and requires it to be stable on its own.
//
// Seed 1021 is a regression seed: its damage leaves two conflicting fatal
// errors in the cost evidence walk, and validating them in map iteration order
// made the read report "invalid client index proof" or "client/hour evidence
// disagrees with raw history" at random, which is what the Fast vs Full fuzz
// observed as a disagreement. A two-entry map starts its walk at a random slot,
// so each read reverses the pair with measured probability ~1/8; the regression
// seed therefore gets extra reads per mode (a reverted fix survives ~(7/8)^32
// of runs per mode), while the other seeds keep the suite fast. The
// deterministic-first-error test pins the ordering mechanism itself, and the
// fixed seed set keeps the cost independent of
// LLMPROXY_INTEGRITY_FUZZ_ITERATIONS.
func TestIntegrityReadVerdictDeterministic(t *testing.T) {
	const (
		deterministicSeeds     = 25
		regressionSeed         = 1021
		readsPerMode           = 8
		regressionReadsPerMode = 32
	)
	seeds := make([]int, 0, deterministicSeeds+1)
	for seed := 0; seed < deterministicSeeds; seed++ {
		seeds = append(seeds, seed)
	}
	seeds = append(seeds, regressionSeed)
	for _, seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		f := buildClientScopeFuzzDB(t, rng, rng.Intn(5))
		reads := readsPerMode
		if seed == regressionSeed {
			reads = regressionReadsPerMode
		}
		for _, client := range []string{"alice", "bob", "carol"} {
			q := integrityQuery{client: client, from: integrityFuzzBase, to: integrityFuzzBase.Add(40 * time.Hour), windowKeys: true, exact: true}
			for _, fast := range []bool{false, true} {
				first := runIntegrityQuery(f.db, q, fast)
				for repeat := 1; repeat < reads; repeat++ {
					next := runIntegrityQuery(f.db, q, fast)
					for i := range first {
						if first[i] != next[i] {
							t.Fatalf("seed %d client %s fast=%v: identical reads disagree (repeat %d)\n first: %s\n next:  %s",
								seed, client, fast, repeat, first[i], next[i])
						}
					}
				}
			}
		}
	}
}
