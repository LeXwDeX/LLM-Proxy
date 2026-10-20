//go:build race

package proxy

// raceEnabled lets allocation-budget tests skip under the race detector,
// whose instrumentation adds allocations unrelated to the code under test.
const raceEnabled = true
