package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// IdleAfter is how long a room must be idle with open work before people hear about it.
const IdleAfter = 10 * time.Minute

// CrashThreshold is how many consecutive CLI exits count as a crash.
const CrashThreshold = 3

// DiskLowRatio is the volume use that raises diskLow.
const DiskLowRatio = 0.8

// OpenWork tells whether an agent has open work: forge issues or PRs
// assigned to it, or unread messages.
type OpenWork interface {
	HasOpenWork(ctx context.Context, workspace, room string) (bool, error)
}

// Tracker turns presence, status reports and pod restarts into events.
type Tracker struct {
	D        *Dispatcher
	OpenWork OpenWork // nil: no forge check (idleWithOpenWork never fires)
	Now      func() time.Time

	mu       sync.Mutex
	idle     map[string]time.Time // key ws/room
	raised   map[string]bool
	restarts map[string]int32
}

// NewTracker returns a tracker sending through d.
func NewTracker(d *Dispatcher, ow OpenWork) *Tracker {
	return &Tracker{D: d, OpenWork: ow, Now: time.Now, idle: map[string]time.Time{}, raised: map[string]bool{}, restarts: map[string]int32{}}
}

// Presence handles one presence event from messaging.
func (t *Tracker) Presence(ctx context.Context, ws, room string, p messagingv1.Presence, st *messagingv1.RoomStatusReport) {
	key := ws + "/" + room
	t.mu.Lock()
	if p == messagingv1.Presence_PRESENCE_IDLE {
		if _, ok := t.idle[key]; !ok {
			t.idle[key] = t.Now()
		}
	} else if p != messagingv1.Presence_PRESENCE_UNSPECIFIED {
		delete(t.idle, key)
		delete(t.raised, key)
		t.mu.Unlock()
		t.D.Clear(ws, room, v1alpha1.EventIdleWithOpenWork)
		t.mu.Lock()
	}
	t.mu.Unlock()
	if st == nil {
		return
	}
	// Disk.
	low := ""
	for _, v := range st.GetVolumes() {
		if v.GetCapacityBytes() > 0 && float64(v.GetUsedBytes()) >= DiskLowRatio*float64(v.GetCapacityBytes()) {
			low = fmt.Sprintf("%s at %d%%", v.GetName(), v.GetUsedBytes()*100/v.GetCapacityBytes())
			break
		}
	}
	if low != "" {
		t.D.Raise(ctx, Event{Kind: v1alpha1.EventDiskLow, Workspace: ws, Room: room, Fingerprint: "low",
			Title: room + ": disk almost full", Body: "Volume " + low + ".", URL: roomURL(ws, room, "storage")})
	} else if len(st.GetVolumes()) > 0 {
		t.D.Clear(ws, room, v1alpha1.EventDiskLow)
	}
	// CLI crashes.
	if st.GetCrashCount() >= CrashThreshold {
		t.D.Raise(ctx, Event{Kind: v1alpha1.EventRoomCrashed, Workspace: ws, Room: room, Fingerprint: "cli",
			Title: room + ": agent keeps crashing", Body: fmt.Sprintf("The CLI exited %d times in a row.", st.GetCrashCount()),
			URL: roomURL(ws, room, "logs")})
	} else if st.GetCrashCount() == 0 {
		t.D.Clear(ws, room, v1alpha1.EventRoomCrashed)
	}
}

// PodRestarts handles a room pod's restart count, as the fleet sees it.
func (t *Tracker) PodRestarts(ctx context.Context, ws, room string, restarts int32) {
	key := ws + "/" + room
	t.mu.Lock()
	prev, seen := t.restarts[key]
	t.restarts[key] = restarts
	t.mu.Unlock()
	if seen && restarts > prev {
		t.D.Raise(ctx, Event{Kind: v1alpha1.EventRoomCrashed, Workspace: ws, Room: room, Fingerprint: fmt.Sprint("pod-", restarts),
			Title: room + ": room restarted", Body: fmt.Sprintf("The room's container restarted (%d restarts).", restarts),
			URL: roomURL(ws, room, "logs")})
	}
}

// Tick raises idleWithOpenWork for rooms idle for IdleAfter with open work.
func (t *Tracker) Tick(ctx context.Context) {
	if t.OpenWork == nil {
		return
	}
	now := t.Now()
	t.mu.Lock()
	var due []string
	for key, since := range t.idle {
		if !t.raised[key] && now.Sub(since) >= IdleAfter {
			due = append(due, key)
		}
	}
	t.mu.Unlock()
	for _, key := range due {
		ws, room := splitKey(key)
		open, err := t.OpenWork.HasOpenWork(ctx, ws, room)
		if err != nil || !open {
			continue
		}
		t.mu.Lock()
		since, still := t.idle[key]
		if still {
			t.raised[key] = true
		}
		t.mu.Unlock()
		if !still {
			continue
		}
		t.D.Raise(ctx, Event{Kind: v1alpha1.EventIdleWithOpenWork, Workspace: ws, Room: room, Fingerprint: since.String(),
			Title: room + " is idle with open work", Body: "It has assigned issues, pull requests or unread messages.",
			URL: roomURL(ws, room, "activity")})
	}
}

// Loop runs Tick every minute.
func (t *Tracker) Loop(ctx context.Context) {
	tk := time.NewTicker(time.Minute)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.Tick(ctx)
		}
	}
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func roomURL(ws, room, tab string) string {
	return "/workspaces/" + ws + "/rooms/" + room + "/" + tab
}
