//go:build windows

package nosql

// mmapReservationSupported is false on Windows: bbolt truncates the file to
// InitialMmapSize when opening there, so a reservation would grow the file on
// disk and double it again on every restart.
const mmapReservationSupported = false
