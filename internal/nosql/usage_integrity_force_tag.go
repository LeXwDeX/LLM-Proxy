//go:build forcefastintegrity

package nosql

// Built only with -tags forcefastintegrity (test runs, see the Makefile target
// test-fast-integrity): every integrity read classifies its own transaction
// and takes the windowed fast path, so the whole suite checks Fast == Full.
func init() { integrityForceFast.Store(true) }
