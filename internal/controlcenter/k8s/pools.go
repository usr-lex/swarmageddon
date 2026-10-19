package k8s

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// ConditionPoolDiskLow is the RunnerPool condition the operator sets when no
// schedulable node has the pool's minFreeDiskGB free, measured from
// kubelet's stats summary (internal/operator/runnerpool).
const ConditionPoolDiskLow = "DiskLow"

// ConditionPoolNodeDiskLow is set when a node running the workspace's pods
// has under 15% of its disk free (docs/design.md §12).
const ConditionPoolNodeDiskLow = "NodeDiskLow"

// NodeDiskLow returns, for each workspace with a runner pool, why node disk
// is low, or "" when it isn't. The control center can't read nodes, so this
// is what the operator measured for the pool (docs/design.md §12).
func (c *Client) NodeDiskLow(ctx context.Context) (map[string]string, error) {
	wss, err := c.Workspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, w := range wss {
		var l v1alpha1.RunnerPoolList
		if err := c.C.List(ctx, &l, client.InNamespace(v1alpha1.WorkspaceNamespace(w.Name))); err != nil || len(l.Items) == 0 {
			continue
		}
		out[w.Name] = ""
		for _, p := range l.Items {
			cond := meta.FindStatusCondition(p.Status.Conditions, ConditionPoolNodeDiskLow)
			if cond == nil || cond.Status != "True" {
				cond = meta.FindStatusCondition(p.Status.Conditions, ConditionPoolDiskLow)
			}
			if cond != nil && cond.Status == "True" {
				out[w.Name] = cond.Message
				if cond.Message == "" {
					out[w.Name] = "a node is below the runner pool's minimum free disk"
				}
				break
			}
		}
	}
	return out, nil
}
