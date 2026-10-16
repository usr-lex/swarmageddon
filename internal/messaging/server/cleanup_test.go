package server

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"
)

// A deleted workspace's streams go away, but only after it has been missing
// from the directory on two scans in a row.
func TestRemovesDeletedWorkspaceStreams(t *testing.T) {
	dir := testDirectory()
	reps := startCluster(t, 3, dir)
	ctx := context.Background()
	st := reps[0].st

	waitFor := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			have, err := st.WorkspaceStreams(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(have, "acme") == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("streams of acme present = %v, want %v", !want, want)
	}

	waitFor(true) // Run creates them
	srv := New(dir, st, slog.New(slog.DiscardHandler))
	missing := map[string]bool{}
	srv.removeDeleted(ctx, missing)
	if len(missing) != 0 {
		t.Fatalf("a live workspace was marked missing: %v", missing)
	}

	dir.Delete("acme")
	for _, r := range reps {
		r.stop() // stop Run, so only this test's scans act
	}
	srv.removeDeleted(ctx, missing)
	have, _ := st.WorkspaceStreams(ctx)
	if !slices.Contains(have, "acme") {
		t.Fatal("streams removed after one scan; want two")
	}
	srv.removeDeleted(ctx, missing)
	waitFor(false)
}
