//go:build race

package nosql

// raceEnabled lets wall-clock liveness tests scale their deadlines under the
// race detector, which slows instrumented code several times over.
const raceEnabled = true
