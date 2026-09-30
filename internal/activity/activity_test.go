package activity

import "testing"

func TestBoundedHistoryPreservesTotals(t *testing.T) {
	store := New(3)
	for i := 0; i < 10; i++ {
		store.Add("write", "file", nil)
	}
	history, counts, errors := store.Snapshot()
	if len(history) != 3 || counts["write"] != 10 || errors != 0 {
		t.Fatalf("history=%d counts=%v errors=%d", len(history), counts, errors)
	}
}
