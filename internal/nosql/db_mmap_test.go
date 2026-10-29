package nosql

import (
	"os"
	"path/filepath"
	"strconv"
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
		{"small file uses floor", 200 << 20, floor},
		{"just below floor after doubling", floor/2 - 1, floor},
		{"exactly half the floor", floor / 2, floor},
		{"large file doubles, rounded up to granularity", floor/2 + 1, floor + mmapGranularity},
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
	if got <= 0 {
		t.Fatalf("initialMmapSize overflowed: %d", got)
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
	path := filepath.Join(t.TempDir(), "existing.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
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
	if got < mmapFloor32 {
		t.Fatalf("InitialMmapSize = %d, want at least the floor", got)
	}
}
