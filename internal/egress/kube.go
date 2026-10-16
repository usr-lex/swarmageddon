package egress

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WatchTable keeps p's table in sync with ConfigMap egress-allowlist in
// namespace ns until ctx ends. It returns a function that reports whether
// the first list has completed (a missing ConfigMap is an empty table).
func WatchTable(ctx context.Context, cs kubernetes.Interface, ns string, p *Proxy, log *slog.Logger) func() bool {
	lw := cache.NewListWatchFromClient(cs.CoreV1().RESTClient(), "configmaps", ns,
		fields.OneTermEqualSelector("metadata.name", ConfigMapName))
	set := func(obj any) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
		t, errs := Parse(cm.Data)
		for _, err := range errs {
			log.Warn("allowlist", "err", err)
		}
		p.SetTable(t)
		log.Info("allowlist updated", "rooms", t.Len())
	}
	_, ctrl := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: lw,
		ObjectType:    &corev1.ConfigMap{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    set,
			UpdateFunc: func(_, n any) { set(n) },
			DeleteFunc: func(any) { p.SetTable(nil) },
		},
	})
	go ctrl.Run(ctx.Done())
	return ctrl.HasSynced
}
