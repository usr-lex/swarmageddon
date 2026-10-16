// Package keytable is the table that tells the gateway guard which room a
// gateway key belongs to (ADR 0011). The operator keeps it in ConfigMap
// warden-keys in the platform namespace: one data key per room,
// "<workspace>.<room>", holding an Entry as JSON. Each room's controller
// merge-patches only its own data key, so rooms never conflict. Keys are
// stored as SHA-256 hashes, never in the clear.
package keytable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// ConfigMapName is the table's ConfigMap in the platform namespace.
const ConfigMapName = "warden-keys"

// Annotations on a Room the warden quarantined. The operator shows the
// reason in the Quarantined condition.
const (
	AnnotationQuarantineReason = "swarmageddon.dev/quarantine-reason"
	AnnotationQuarantinedBy    = "swarmageddon.dev/quarantined-by"
	AnnotationQuarantinedAt    = "swarmageddon.dev/quarantined-at"
)

// Key kinds.
const (
	KindMain    = "main"    // the room's CLI (gateway_key)
	KindAdvisor = "advisor" // the room's advisor (advisor_key)
	KindWarden  = "warden"  // the room's in-room warden (warden_key)
)

// Entry is one room's row.
type Entry struct {
	// Room is the room's address, <room>@<workspace>.
	Room string `json:"room"`
	// Keys maps Hash(key) to its kind.
	Keys map[string]string `json:"keys"`
	// Quarantined mirrors Room.spec.quarantined.
	Quarantined bool `json:"quarantined,omitempty"`
	// Mode is the room's effective warden mode: off, monitor or gate.
	Mode string `json:"mode,omitempty"`
	// Model is the warden model; empty: rules only.
	Model string `json:"model,omitempty"`
	// QuarantineOnHigh quarantines on high verdicts, not only critical ones.
	QuarantineOnHigh bool `json:"quarantineOnHigh,omitempty"`
	// Rules are the RoleTemplate's wardenRules, run after the built-ins.
	Rules []v1.WardenRule `json:"rules,omitempty"`
}

// Name and Workspace split the room's address.
func (e *Entry) Name() string      { n, _, _ := strings.Cut(e.Room, "@"); return n }
func (e *Entry) Workspace() string { _, w, _ := strings.Cut(e.Room, "@"); return w }

// DataKey is a room's key in the ConfigMap.
func DataKey(workspace, room string) string { return workspace + "." + room }

// Hash is the hex SHA-256 of a gateway key (as LiteLLM stores it).
func Hash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

// Marshal encodes an entry for the ConfigMap.
func (e *Entry) Marshal() string {
	b, _ := json.Marshal(e) // map keys sort: stable
	return string(b)
}

// Ref is what a key resolves to.
type Ref struct {
	Entry *Entry
	Kind  string
}

// Table is the parsed ConfigMap.
type Table struct {
	byHash map[string]Ref
	rooms  map[string]*Entry
}

// Parse reads the ConfigMap's data. Bad rows are skipped and reported.
func Parse(data map[string]string) (*Table, []error) {
	t := &Table{byHash: map[string]Ref{}, rooms: map[string]*Entry{}}
	var errs []error
	for k, v := range data {
		var e Entry
		if err := json.Unmarshal([]byte(v), &e); err != nil || e.Room == "" {
			errs = append(errs, fmt.Errorf("warden-keys %s: bad entry: %v", k, err))
			continue
		}
		t.rooms[e.Room] = &e
		for h, kind := range e.Keys {
			t.byHash[strings.ToLower(h)] = Ref{Entry: &e, Kind: kind}
		}
	}
	return t, errs
}

// Lookup resolves a raw key.
func (t *Table) Lookup(key string) (Ref, bool) {
	if t == nil || key == "" {
		return Ref{}, false
	}
	r, ok := t.byHash[Hash(key)]
	return r, ok
}

// Room returns a room's entry by address.
func (t *Table) Room(address string) *Entry {
	if t == nil {
		return nil
	}
	return t.rooms[address]
}

// Len is the number of rooms.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rooms)
}
