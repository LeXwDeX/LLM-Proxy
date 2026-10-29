//go:build !windows

package nosql

// mmapReservationSupported reports whether OpenDB may set bbolt's
// InitialMmapSize. Reserving only costs address space here.
const mmapReservationSupported = true
