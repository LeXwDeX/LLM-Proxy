package tracestore

import "testing"

func TestStoreEnabledReportsWhetherRecordsAreKept(t *testing.T) {
	var nilStore *Store
	if nilStore.Enabled() {
		t.Fatal("nil store reported enabled")
	}
	if New(Config{Enabled: false}, nil).Enabled() {
		t.Fatal("disabled store reported enabled")
	}
	store := New(Config{Enabled: true, RingBufferSize: 1, ChannelBuffer: 1}, nil)
	if !store.Enabled() {
		t.Fatal("enabled store reported disabled")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store.Enabled() {
		t.Fatal("closed store still reported enabled")
	}
}
