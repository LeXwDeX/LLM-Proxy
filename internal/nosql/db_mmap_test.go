package nosql

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestInitialMmapSize(t *testing.T) {
	floor := int64(mmapFloor64)
	if strconv.IntSize == 32 {
		floor = mmapFloor32
	}
	cases := []struct {
		name string
		size int64
		want int64
	}{
		{"missing file uses floor", 0, floor},
		{"small file adds the floor", 200 << 20, floor + 256<<20},
		{"file just below floor adds the floor", floor - 1, 2 * floor},
		{"file exactly at floor adds the floor", floor, 2 * floor},
		{"file above floor adds its own size, rounded up to granularity", floor + 1, 2*floor + mmapGranularity},
		{"3 GiB doubles", 3 << 30, 6 << 30},
	}
	for _, tc := range cases {
		if strconv.IntSize == 32 && tc.want > 1<<30 {
			continue // not representable on 32-bit; covered by the clamp test
		}
		t.Run(tc.name, func(t *testing.T) {
			got := int64(initialMmapSize(tc.size))
			if got != tc.want {
				t.Fatalf("initialMmapSize(%d) = %d, want %d", tc.size, got, tc.want)
			}
			if got%mmapGranularity != 0 {
				t.Fatalf("%d is not a multiple of the granularity", got)
			}
		})
	}
}

func TestInitialMmapSizeClampsToPlatformInt(t *testing.T) {
	got := initialMmapSize(1 << 62)
	maxInt := int64(^uint(0) >> 1)
	if got <= 0 || int64(got) > maxInt {
		t.Fatalf("initialMmapSize overflowed: %d", got)
	}
	if int64(got)%mmapGranularity != 0 {
		t.Fatalf("clamped size %d is not a multiple of the granularity", got)
	}
}

func TestOpenDBFallsBackWhenMmapReservationFails(t *testing.T) {
	orig := boltOpen
	t.Cleanup(func() { boltOpen = orig })
	var sizes []int
	boltOpen = func(path string, mode os.FileMode, opts *bolt.Options) (*bolt.DB, error) {
		sizes = append(sizes, opts.InitialMmapSize)
		if opts.InitialMmapSize > 0 {
			return nil, syscall.ENOMEM
		}
		return orig(path, mode, opts)
	}
	db, err := OpenDB(filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatalf("OpenDB should fall back, got %v", err)
	}
	defer db.Close()
	if len(sizes) != 2 || sizes[0] <= 0 || sizes[1] != 0 {
		t.Fatalf("open attempts InitialMmapSize = %v, want [>0 0]", sizes)
	}
}

func TestOpenDBDoesNotMaskOtherOpenErrors(t *testing.T) {
	orig := boltOpen
	t.Cleanup(func() { boltOpen = orig })
	calls := 0
	boltOpen = func(string, os.FileMode, *bolt.Options) (*bolt.DB, error) {
		calls++
		return nil, bolt.ErrTimeout
	}
	if _, err := OpenDB(filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("non-mmap error retried %d times", calls)
	}
}

func TestOpenDBReservationAppliesToExistingLargeFile(t *testing.T) {
	if !mmapReservationSupported {
		t.Skip("mmap reservation disabled on this platform")
	}
	floor := int64(mmapFloor64)
	if strconv.IntSize == 32 {
		floor = mmapFloor32
	}
	path := filepath.Join(t.TempDir(), "existing.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Sparse growth: larger than floor/2 and above the floor itself, so the
	// reservation must follow the file size rather than sit at the floor.
	size := floor + 100<<20
	if strconv.IntSize == 32 {
		size = floor/2 + 8<<20
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Skipf("cannot create sparse file: %v", err)
	}
	f.Close()

	orig := boltOpen
	t.Cleanup(func() { boltOpen = orig })
	var got int
	boltOpen = func(p string, m os.FileMode, o *bolt.Options) (*bolt.DB, error) {
		got = o.InitialMmapSize
		return orig(p, m, o)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if want := initialMmapSize(size); got != want {
		t.Fatalf("InitialMmapSize = %d, want initialMmapSize(%d) = %d", got, size, want)
	}
	if int64(got) <= floor {
		t.Fatalf("InitialMmapSize = %d, want above the floor %d for a %d byte file", got, floor, size)
	}
}

func TestReservedMmapSizePerPlatform(t *testing.T) {
	for _, size := range []int64{0, 200 << 20, 3 << 30} {
		want := 0
		if mmapReservationSupported {
			want = initialMmapSize(size)
		}
		if got := reservedMmapSize(size); got != want {
			t.Fatalf("reservedMmapSize(%d) = %d, want %d (supported=%v)", size, got, want, mmapReservationSupported)
		}
	}
}

func TestCheckMmapHeadroomWarnsOncePastThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headroom.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Ample headroom: no warning.
	mmapReservations.Store(path, &mmapReservationState{reserved: fi.Size() * 10})
	if CheckMmapHeadroom(path, logger) || buf.Len() != 0 {
		t.Fatalf("warned with ample headroom: %q", buf.String())
	}
	// File above 80% of the reservation: warn exactly once.
	mmapReservations.Store(path, &mmapReservationState{reserved: fi.Size() * 11 / 10})
	if !CheckMmapHeadroom(path, logger) {
		t.Fatal("expected a warning past the threshold")
	}
	if CheckMmapHeadroom(path, logger) {
		t.Fatal("warned twice")
	}
	if strings.Count(buf.String(), "close to its reserved mmap size") != 1 {
		t.Fatalf("log = %q", buf.String())
	}
	// No reservation recorded (fallback / Windows): never warn.
	mmapReservations.Store(path, &mmapReservationState{reserved: 0})
	if CheckMmapHeadroom(path, logger) {
		t.Fatal("warned without a reservation")
	}
}
