package main

import "testing"

func TestUsageRetentionDaysKeepsMonthlyCalibrationHistory(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int
		warning bool
	}{
		{"", 90, false},
		{"0", 0, false},
		{"120", 120, false},
		{"35", 35, false},
		{"7", 35, true},
		{"-1", 90, true},
		{"abc", 90, true},
	} {
		got, warning := usageRetentionDays(tc.raw)
		if got != tc.want || (warning != "") != tc.warning {
			t.Errorf("usageRetentionDays(%q) = %d, %q; want %d, warning=%v", tc.raw, got, warning, tc.want, tc.warning)
		}
	}
}
