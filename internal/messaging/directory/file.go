package directory

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// LoadFile reads Workspace, Room and RoleTemplate manifests (multi-document
// YAML, as for kubectl apply) into a Static directory. Rooms and role
// templates belong to the workspace of their namespace (ws-<name>). Other
// kinds are ignored. For development without a cluster.
func LoadFile(path string) (*Static, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var (
		workspaces []v1alpha1.Workspace
		rooms      = map[string][]v1alpha1.Room{}
		roles      = map[string][]v1alpha1.RoleTemplate{}
	)
	for i, doc := range docSep.Split(string(data), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
		}
		ws := strings.TrimPrefix(meta.Metadata.Namespace, v1alpha1.WorkspaceNamespacePrefix)
		switch meta.Kind {
		case "Workspace":
			var w v1alpha1.Workspace
			if err := yaml.UnmarshalStrict([]byte(doc), &w); err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
			}
			workspaces = append(workspaces, w)
		case "Room":
			var r v1alpha1.Room
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
			}
			rooms[ws] = append(rooms[ws], r)
		case "RoleTemplate":
			var r v1alpha1.RoleTemplate
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, i+1, err)
			}
			roles[ws] = append(roles[ws], r)
		}
	}
	s := NewStatic()
	for i := range workspaces {
		w := &workspaces[i]
		s.Set(FromObjects(w, rooms[w.Name], roles[w.Name]))
	}
	return s, nil
}
