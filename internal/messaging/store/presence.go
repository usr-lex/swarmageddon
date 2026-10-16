package store

import (
	"context"
	"errors"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// PutPresence records name's presence (also a heartbeat that renews the
// 90 s TTL) and returns the previous one (OFFLINE if none).
func (s *Store) PutPresence(ctx context.Context, ws, name string, p messagingv1.Presence) (messagingv1.Presence, error) {
	prev := s.Presence(ctx, ws, name)
	data, err := proto.Marshal(&messagingv1.PresenceEvent{Address: name + "@" + ws, Presence: p, At: now()})
	if err != nil {
		return prev, err
	}
	_, err = s.presence.Put(ctx, ws+"."+name, data)
	return prev, err
}

// Presence returns name's presence; OFFLINE if it has lapsed.
func (s *Store) Presence(ctx context.Context, ws, name string) messagingv1.Presence {
	e, err := s.presence.Get(ctx, ws+"."+name)
	if err != nil {
		return messagingv1.Presence_PRESENCE_OFFLINE
	}
	var ev messagingv1.PresenceEvent
	if proto.Unmarshal(e.Value(), &ev) != nil {
		return messagingv1.Presence_PRESENCE_OFFLINE
	}
	return ev.Presence
}

// PutStatus stores a room's latest status report.
func (s *Store) PutStatus(ctx context.Context, ws, name string, st *messagingv1.RoomStatusReport) error {
	data, err := proto.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.status.Put(ctx, ws+"."+name, data)
	return err
}

// WatchPresence streams presence and status changes for the given
// workspaces (all if empty) to emit: first the current state, then changes,
// including lapses as OFFLINE. Addresses in known that have no presence get
// an initial OFFLINE event.
func (s *Store) WatchPresence(ctx context.Context, workspaces []string, known []string, emit func(*messagingv1.PresenceEvent) error) error {
	keys := []string{">"}
	if len(workspaces) > 0 {
		keys = nil
		for _, ws := range workspaces {
			keys = append(keys, ws+".*")
		}
	}
	pw, err := s.presence.WatchFiltered(ctx, keys)
	if err != nil {
		return err
	}
	defer pw.Stop()
	sw, err := s.status.WatchFiltered(ctx, keys)
	if err != nil {
		return err
	}
	defer sw.Stop()

	type state struct {
		presence messagingv1.Presence
		status   *messagingv1.RoomStatusReport
	}
	states := map[string]*state{}
	get := func(key string) (string, *state) {
		ws, name, _ := strings.Cut(key, ".")
		addr := name + "@" + ws
		st := states[addr]
		if st == nil {
			st = &state{presence: messagingv1.Presence_PRESENCE_OFFLINE}
			states[addr] = st
		}
		return addr, st
	}
	pInit, sInit := false, false
	sentKnown := false
	for {
		var (
			e   jetstream.KeyValueEntry
			ok  bool
			isP bool
		)
		select {
		case <-ctx.Done():
			return nil
		case e, ok = <-pw.Updates():
			isP = true
		case e, ok = <-sw.Updates():
		}
		if !ok {
			return errors.New("presence watch closed")
		}
		if e == nil { // end of initial values
			if isP {
				pInit = true
			} else {
				sInit = true
			}
			if pInit && sInit && !sentKnown {
				sentKnown = true
				for _, addr := range known {
					if _, seen := states[addr]; !seen {
						states[addr] = &state{presence: messagingv1.Presence_PRESENCE_OFFLINE}
						if err := emit(&messagingv1.PresenceEvent{Address: addr, Presence: messagingv1.Presence_PRESENCE_OFFLINE, At: now()}); err != nil {
							return err
						}
					}
				}
			}
			continue
		}
		addr, st := get(e.Key())
		ev := &messagingv1.PresenceEvent{Address: addr, At: now()}
		deleted := e.Operation() == jetstream.KeyValueDelete || e.Operation() == jetstream.KeyValuePurge
		if isP {
			var pe messagingv1.PresenceEvent
			if deleted || proto.Unmarshal(e.Value(), &pe) != nil {
				st.presence = messagingv1.Presence_PRESENCE_OFFLINE
			} else {
				st.presence = pe.Presence
				ev.At = pe.At
			}
		} else if !deleted {
			var rs messagingv1.RoomStatusReport
			if proto.Unmarshal(e.Value(), &rs) == nil {
				st.status = &rs
			}
		}
		ev.Presence, ev.Status = st.presence, st.status
		if err := emit(ev); err != nil {
			return err
		}
	}
}
