// Package k8s is the control center's access to Kubernetes (docs/design.md
// §8.2, §11.3). It writes only swarmageddon custom resources, reads pods,
// logs and metrics, and touches Secrets only in the system namespace by the
// names the control center owns (AllowedSecret), plus each workspace's
// room-secrets Secret, which the operator creates and grants by name.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Names of the control center's own Secrets in the system namespace.
const (
	SecretSetupToken         = "setup-token"
	SecretLocalAdmin         = "local-admin"
	SecretSession            = "control-center-session"
	SecretDB                 = "control-center-db"
	SecretVAPID              = "vapid"
	SecretSAML               = "control-center-saml"    // SP signing key and certificate
	SecretGateway            = "control-center-gateway" // key masterKey: a gateway key for reading spend
	SecretForgeReader        = "forge-reader"           // key token: read-only Forgejo token
	SecretForgeWebhookPrefix = "forge-webhook-"         // + workspace: key secret (HMAC for /hooks/forgejo/<ws>)
)

// AllowedSecret reports whether the control center may touch a Secret of
// this name in the system namespace: v1alpha1.ControlCenterSecret, the
// list the chart and the operator grant from.
func AllowedSecret(name string) bool { return v1alpha1.ControlCenterSecret(name) }

// ErrSecretNotAllowed is returned for Secrets outside AllowedSecret.
var ErrSecretNotAllowed = errors.New("the control center may not access this Secret")

// Scheme has the core types and the swarmageddon resources.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	_ = metricsv1beta1.AddToScheme(s)
	return s
}

// Client bundles the clients the control center uses.
type Client struct {
	C       client.Client
	Kube    kubernetes.Interface
	Metrics metricsclient.Interface
	Config  *rest.Config
	// Namespace is the system namespace.
	Namespace string
}

// New builds the clients from a REST config.
func New(cfg *rest.Config) (*Client, error) {
	c, err := client.New(cfg, client.Options{Scheme: Scheme()})
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mc, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{C: c, Kube: kube, Metrics: mc, Config: cfg, Namespace: v1alpha1.SystemNamespace}, nil
}

// GetSecret returns an allowed Secret's data; apierrors.IsNotFound tells a missing one.
func (c *Client) GetSecret(ctx context.Context, name string) (map[string][]byte, error) {
	if !AllowedSecret(name) {
		return nil, ErrSecretNotAllowed
	}
	var s corev1.Secret
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, &s); err != nil {
		return nil, err
	}
	return s.Data, nil
}

// ApplySecret creates or replaces an allowed Secret's data. It creates
// first, since a Secret the operator hasn't granted yet can't be read
// (granted.go).
func (c *Client) ApplySecret(ctx context.Context, name string, data map[string][]byte) error {
	if !AllowedSecret(name) {
		return ErrSecretNotAllowed
	}
	err := c.C.Create(ctx, c.ownedSecret(name, data))
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	return whenGranted(func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var s corev1.Secret
			if err := c.C.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, &s); err != nil {
				return err
			}
			s.Data = data
			return c.C.Update(ctx, &s)
		})
	})
}

// CreateSecretIfMissing creates an allowed Secret unless it exists, and
// returns the data that is stored (the existing one wins a race).
func (c *Client) CreateSecretIfMissing(ctx context.Context, name string, data map[string][]byte) (map[string][]byte, error) {
	if !AllowedSecret(name) {
		return nil, ErrSecretNotAllowed
	}
	err := c.C.Create(ctx, c.ownedSecret(name, data))
	if apierrors.IsAlreadyExists(err) {
		var stored map[string][]byte
		err := whenGranted(func() (err error) {
			stored, err = c.GetSecret(ctx, name)
			return err
		})
		return stored, err
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ErrConfigMapNotAllowed is returned for ConfigMaps outside
// v1alpha1.ControlCenterConfigMap.
var ErrConfigMapNotAllowed = errors.New("the control center may not access this ConfigMap")

// ApplyConfigMap creates or replaces a ConfigMap in the system namespace
// (used for pasted SAML IdP metadata, saml-<provider>). Like ApplySecret,
// it creates first.
func (c *Client) ApplyConfigMap(ctx context.Context, name string, data map[string]string) error {
	if !v1alpha1.ControlCenterPrefixedConfigMap(name) {
		return errors.New("the control center only writes saml-* ConfigMaps")
	}
	err := c.C.Create(ctx, c.ownedConfigMap(name, data))
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	return whenGranted(func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cm corev1.ConfigMap
			if err := c.C.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, &cm); err != nil {
				return err
			}
			cm.Data = data
			return c.C.Update(ctx, &cm)
		})
	})
}

// GetConfigMap reads an allowed ConfigMap in the system namespace.
func (c *Client) GetConfigMap(ctx context.Context, name string) (map[string]string, error) {
	if !v1alpha1.ControlCenterConfigMap(name) {
		return nil, ErrConfigMapNotAllowed
	}
	var cm corev1.ConfigMap
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, &cm); err != nil {
		return nil, err
	}
	return cm.Data, nil
}

// Platform returns the singleton Platform.
func (c *Client) Platform(ctx context.Context) (*v1alpha1.Platform, error) {
	var p v1alpha1.Platform
	if err := c.C.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdatePlatform applies mutate to the Platform's latest version, retrying
// on conflicts, and returns the result.
func (c *Client) UpdatePlatform(ctx context.Context, mutate func(*v1alpha1.Platform) error) (*v1alpha1.Platform, error) {
	var out *v1alpha1.Platform
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p, err := c.Platform(ctx)
		if err != nil {
			return err
		}
		if err := mutate(p); err != nil {
			return err
		}
		if err := c.C.Update(ctx, p); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// SetAdminClaimed records status.adminClaimed=true on the Platform's status
// subresource; the operator then deletes the setup token.
func (c *Client) SetAdminClaimed(ctx context.Context) error {
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"status":{"adminClaimed":true}}`))
	return c.C.Status().Patch(ctx, p, patch)
}

// Workspaces lists every Workspace.
func (c *Client) Workspaces(ctx context.Context) ([]v1alpha1.Workspace, error) {
	var l v1alpha1.WorkspaceList
	if err := c.C.List(ctx, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Workspace returns one Workspace.
func (c *Client) Workspace(ctx context.Context, name string) (*v1alpha1.Workspace, error) {
	var w v1alpha1.Workspace
	if err := c.C.Get(ctx, types.NamespacedName{Name: name}, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// Rooms lists a workspace's Rooms.
func (c *Client) Rooms(ctx context.Context, workspace string) ([]v1alpha1.Room, error) {
	var l v1alpha1.RoomList
	if err := c.C.List(ctx, &l, client.InNamespace(v1alpha1.WorkspaceNamespace(workspace))); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Room returns one Room.
func (c *Client) Room(ctx context.Context, workspace, name string) (*v1alpha1.Room, error) {
	var r v1alpha1.Room
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(workspace), Name: name}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// RoleTemplate returns a RoleTemplate in a workspace.
func (c *Client) RoleTemplate(ctx context.Context, workspace, name string) (*v1alpha1.RoleTemplate, error) {
	var t v1alpha1.RoleTemplate
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(workspace), Name: name}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// RoleTemplates lists a workspace's RoleTemplates.
func (c *Client) RoleTemplates(ctx context.Context, workspace string) ([]v1alpha1.RoleTemplate, error) {
	var l v1alpha1.RoleTemplateList
	if err := c.C.List(ctx, &l, client.InNamespace(v1alpha1.WorkspaceNamespace(workspace))); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// RoomSecretsName is the Secret in each workspace namespace that holds values
// for rooms' secretEnv and MCP valueFrom (docs/design.md §6.6, §11.3).
const RoomSecretsName = "room-secrets"

// ErrRoomSecretsMissing means the operator hasn't created room-secrets yet.
var ErrRoomSecretsMissing = errors.New("the workspace's room-secrets Secret is not there yet; try again in a moment")

// RoomSecretKeys returns the keys of a workspace's room-secrets Secret.
func (c *Client) RoomSecretKeys(ctx context.Context, workspace string) ([]string, error) {
	var s corev1.Secret
	err := c.C.Get(ctx, types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(workspace), Name: RoomSecretsName}, &s)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	return keys, nil
}

// SetRoomSecretKey sets (value != nil) or removes (nil) one key of a
// workspace's room-secrets Secret.
func (c *Client) SetRoomSecretKey(ctx context.Context, workspace, key string, value []byte) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s corev1.Secret
		err := c.C.Get(ctx, types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(workspace), Name: RoomSecretsName}, &s)
		if apierrors.IsNotFound(err) {
			return ErrRoomSecretsMissing
		}
		if err != nil {
			return err
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		if value == nil {
			if _, ok := s.Data[key]; !ok {
				return nil
			}
			delete(s.Data, key)
		} else {
			s.Data[key] = value
		}
		return c.C.Update(ctx, &s)
	})
}

// PodName is the pod of a room's StatefulSet.
func PodName(room string) string { return "room-" + room + "-0" }

// RoomPod returns a room's pod.
func (c *Client) RoomPod(ctx context.Context, workspace, room string) (*corev1.Pod, error) {
	var p corev1.Pod
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(workspace), Name: PodName(room)}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RoomPods lists the room pods in a workspace, by room name.
func (c *Client) RoomPods(ctx context.Context, workspace string) (map[string]*corev1.Pod, error) {
	var l corev1.PodList
	if err := c.C.List(ctx, &l, client.InNamespace(v1alpha1.WorkspaceNamespace(workspace)), client.HasLabels{v1alpha1.LabelRoom}); err != nil {
		return nil, err
	}
	out := map[string]*corev1.Pod{}
	for i := range l.Items {
		p := &l.Items[i]
		out[p.Labels[v1alpha1.LabelRoom]] = p
	}
	return out, nil
}

// RoomClaims lists a room's PersistentVolumeClaims (home and engine).
func (c *Client) RoomClaims(ctx context.Context, workspace, room string) ([]corev1.PersistentVolumeClaim, error) {
	ns := v1alpha1.WorkspaceNamespace(workspace)
	var out []corev1.PersistentVolumeClaim
	for _, v := range []string{"home", "engine"} {
		var pvc corev1.PersistentVolumeClaim
		err := c.C.Get(ctx, types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("%s-room-%s-0", v, room)}, &pvc)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, pvc)
	}
	return out, nil
}

// PodMetrics returns a pod's usage from metrics.k8s.io, or nil when the
// metrics API is not available.
func (c *Client) PodMetrics(ctx context.Context, namespace, pod string) (*metricsv1beta1.PodMetrics, error) {
	if c.Metrics == nil {
		return nil, nil
	}
	m, err := c.Metrics.MetricsV1beta1().PodMetricses(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || isNoMetricsAPI(err) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

func isNoMetricsAPI(err error) bool {
	var se *apierrors.StatusError
	if errors.As(err, &se) && se.ErrStatus.Code == 503 {
		return true
	}
	return strings.Contains(err.Error(), "the server could not find the requested resource")
}
