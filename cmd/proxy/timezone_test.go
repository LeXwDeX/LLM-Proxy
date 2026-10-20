package main

import (
	"testing"
	"time"
)

func TestEmbeddedTimezoneDatabase(t *testing.T) {
	tests := []struct {
		name       string
		zone       string
		at         time.Time
		wantOffset int
	}{
		{
			name:       "Asia Shanghai",
			zone:       "Asia/Shanghai",
			at:         time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
			wantOffset: 8 * 60 * 60,
		},
		{
			name:       "New York standard time",
			zone:       "America/New_York",
			at:         time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
			wantOffset: -5 * 60 * 60,
		},
		{
			name:       "New York daylight time",
			zone:       "America/New_York",
			at:         time.Date(2024, time.July, 1, 0, 0, 0, 0, time.UTC),
			wantOffset: -4 * 60 * 60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Fatalf("time.LoadLocation(%q): %v", tt.zone, err)
			}
			_, gotOffset := tt.at.In(loc).Zone()
			if gotOffset != tt.wantOffset {
				t.Fatalf("offset for %q at %s = %d, want %d", tt.zone, tt.at, gotOffset, tt.wantOffset)
			}
		})
	}
}
