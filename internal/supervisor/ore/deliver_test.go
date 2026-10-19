package ore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// A message that arrives while the turn waits on a person is held, not
// steered into the blocked turn and marked read (OpenShift report, issue 2):
// Deliver fails with ErrWaitingOnHuman, so the subscriber keeps it unacked
// and retries, coalescing later messages.
func TestDeliverHoldsWhileWaitingOnAPerson(t *testing.T) {
	d := &Driver{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.init()
	close(d.ready)
	d.client = &Client{} // never called: Deliver returns first
	d.thread, d.activeTurn = "t1", "u1"
	d.pendingN = 1
	if err := d.Deliver(context.Background(), "01M", "hello"); !errors.Is(err, ErrWaitingOnHuman) {
		t.Fatalf("Deliver = %v, want ErrWaitingOnHuman", err)
	}
}
