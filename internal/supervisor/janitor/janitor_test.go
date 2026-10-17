package janitor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPlanThresholds(t *testing.T) {
	c := Config{}
	join := func(p [][]string) string {
		var s []string
		for _, a := range p {
			s = append(s, strings.Join(a, " "))
		}
		return strings.Join(s, "; ")
	}
	low := join(c.Plan(50))
	if !strings.Contains(low, "container prune -f --filter until=24h0m0s") || strings.Contains(low, "image prune -af") {
		t.Errorf("50%%: %s", low)
	}
	mid := join(c.Plan(75))
	if !strings.Contains(mid, "image prune -af") || strings.Contains(mid, "system prune") {
		t.Errorf("75%%: %s", mid)
	}
	high := join(c.Plan(95))
	if strings.Contains(high, "builder prune") {
		t.Errorf("builder prune: Podman's API has no build cache: %s", high)
	}
	if !strings.Contains(high, "system prune -af --volumes") {
		t.Errorf("95%%: %s", high)
	}
	c = Config{ImagesAbovePercent: 40, PruneAfter: time.Hour}
	if p := join(c.Plan(45)); !strings.Contains(p, "image prune -af") || !strings.Contains(p, "until=1h0m0s") {
		t.Errorf("custom: %s", p)
	}
}

func TestRunOnceReportsUsage(t *testing.T) {
	var ran []string
	var got Usage
	j := &Janitor{Config: Config{EngineDir: t.TempDir(), WarnPercent: 101}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnUsage: func(u Usage, low bool) {
			got = u
			if low {
				t.Error("low at 101%")
			}
		},
		run: func(_ context.Context, args ...string) error { ran = append(ran, strings.Join(args, " ")); return nil },
	}
	j.RunOnce(context.Background())
	if len(ran) < 3 || got.Capacity == 0 {
		t.Errorf("ran %v usage %+v", ran, got)
	}
}
