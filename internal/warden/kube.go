package warden

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
)

// WatchTable keeps p's key table in sync with ConfigMap warden-keys in
// namespace ns until ctx ends. It returns a function that reports whether
// the first list has completed (a missing ConfigMap is an empty table).
func WatchTable(ctx context.Context, cs kubernetes.Interface, ns string, p *Proxy, log *slog.Logger) func() bool {
	lw := cache.NewListWatchFromClient(cs.CoreV1().RESTClient(), "configmaps", ns,
		fields.OneTermEqualSelector("metadata.name", keytable.ConfigMapName))
	set := func(obj any) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
		t, errs := keytable.Parse(cm.Data)
		for _, err := range errs {
			log.Warn("key table", "err", err)
		}
		p.SetTable(t)
		log.Info("key table updated", "rooms", t.Len())
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

// RoomQuarantiner patches Room.spec.quarantined with a narrowly scoped
// ClusterRole (get and patch on rooms).
type RoomQuarantiner struct {
	Client client.Client
	Now    func() time.Time
}

// Quarantine implements Quarantiner.
func (q *RoomQuarantiner) Quarantine(ctx context.Context, workspace, room, reason string) error {
	now := time.Now
	if q.Now != nil {
		now = q.Now
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			keytable.AnnotationQuarantineReason: clipTo(reason, 1000),
			keytable.AnnotationQuarantinedBy:    "warden",
			keytable.AnnotationQuarantinedAt:    now().UTC().Format(time.RFC3339),
		}},
		"spec": map[string]any{"quarantined": true},
	})
	r := &v1.Room{}
	r.Namespace, r.Name = v1.WorkspaceNamespace(workspace), room
	return q.Client.Patch(ctx, r, client.RawPatch(types.MergePatchType, patch))
}

// MessagingReporter reports verdicts with the warden's client certificate
// (CN=warden). Certificate files are re-read on each handshake, so a renewed
// Secret is picked up without a restart.
type MessagingReporter struct {
	client messagingv1connect.MessagingServiceClient
}

// NewMessagingReporter connects to messaging at url.
func NewMessagingReporter(url, certFile, keyFile, caFile string) (*MessagingReporter, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s", caFile)
	}
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(certFile, keyFile)
				return &c, err
			},
		},
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	return &MessagingReporter{client: messagingv1connect.NewMessagingServiceClient(hc, url)}, nil
}

// Report implements Reporter.
func (m *MessagingReporter) Report(ctx context.Context, v *messagingv1.Verdict) error {
	_, err := m.client.ReportVerdict(ctx, connect.NewRequest(&messagingv1.ReportVerdictRequest{Verdict: v}))
	return err
}
