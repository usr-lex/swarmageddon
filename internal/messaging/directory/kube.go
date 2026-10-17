package directory

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/tokenhash"
)

// Kube is a Directory backed by an informer cache of Workspace, Room and
// RoleTemplate resources, and of each workspace's messaging-token-hashes
// ConfigMap (no other ConfigMaps). It never writes to the API server.
type Kube struct {
	cache  cache.Cache
	log    *slog.Logger
	synced atomic.Bool

	mu     sync.Mutex
	tokens map[string]parsedTokens // by namespace
}

type parsedTokens struct {
	version string
	set     tokenhash.Set
}

// NewKube creates the cache. Call Start to run it.
func NewKube(cfg *rest.Config, log *slog.Logger) (*Kube, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, ByObject: map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}: {Field: fields.OneTermEqualSelector("metadata.name", tokenhash.ConfigMapName)},
	}})
	if err != nil {
		return nil, fmt.Errorf("directory cache: %w", err)
	}
	return &Kube{cache: c, log: log, tokens: map[string]parsedTokens{}}, nil
}

// Start runs the informers until ctx ends. It registers the informers first,
// then marks the directory ready once they have synced.
func (k *Kube) Start(ctx context.Context) error {
	for _, obj := range []client.Object{&v1alpha1.Workspace{}, &v1alpha1.Room{}, &v1alpha1.RoleTemplate{}, &corev1.ConfigMap{}} {
		if _, err := k.cache.GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("informer for %T: %w", obj, err)
		}
	}
	go func() {
		if k.cache.WaitForCacheSync(ctx) {
			k.synced.Store(true)
			k.log.Info("directory synced")
		}
	}()
	return k.cache.Start(ctx)
}

// Ready implements Directory.
func (k *Kube) Ready() bool { return k.synced.Load() }

// Workspace implements Directory.
func (k *Kube) Workspace(name string) (*Workspace, bool) {
	ctx := context.Background()
	var ws v1alpha1.Workspace
	if err := k.cache.Get(ctx, client.ObjectKey{Name: name}, &ws); err != nil {
		return nil, false
	}
	ns := v1alpha1.WorkspaceNamespace(name)
	var rooms v1alpha1.RoomList
	if err := k.cache.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
		k.log.Warn("list rooms", "workspace", name, "err", err)
	}
	var roles v1alpha1.RoleTemplateList
	if err := k.cache.List(ctx, &roles, client.InNamespace(ns)); err != nil {
		k.log.Warn("list role templates", "workspace", name, "err", err)
	}
	out := FromObjects(&ws, rooms.Items, roles.Items)
	out.TokenHashes = k.tokenHashes(ctx, ns)
	return out, true
}

// tokenHashes is the namespace's token digest set, parsed once per
// ConfigMap version.
func (k *Kube) tokenHashes(ctx context.Context, ns string) tokenhash.Set {
	var cm corev1.ConfigMap
	if err := k.cache.Get(ctx, client.ObjectKey{Namespace: ns, Name: tokenhash.ConfigMapName}, &cm); err != nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if p, ok := k.tokens[ns]; ok && p.version == cm.ResourceVersion {
		return p.set
	}
	set := tokenhash.Parse(cm.Data[tokenhash.DataKey])
	k.tokens[ns] = parsedTokens{version: cm.ResourceVersion, set: set}
	return set
}

// Workspaces implements Directory.
func (k *Kube) Workspaces() []string {
	var list v1alpha1.WorkspaceList
	if err := k.cache.List(context.Background(), &list); err != nil {
		k.log.Warn("list workspaces", "err", err)
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for _, w := range list.Items {
		if w.DeletionTimestamp == nil {
			names = append(names, w.Name)
		}
	}
	sort.Strings(names)
	return names
}
