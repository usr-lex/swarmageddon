package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// welcome is the chart's welcome Job: it waits until the operator has
// published the control center's URL and the setup token, prints how to get
// in, and exits. `kubectl logs -f job/welcome` shows it after an install.
func welcome(timeout time.Duration) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fmt.Println("Waiting for swarmageddon's control center...")
	for {
		done, err := printWelcome(ctx, c, os.Stdout)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the control center wasn't ready after %s; see `kubectl -n %s logs deploy/swarmageddon-operator`",
				timeout, v1alpha1.SystemNamespace)
		case <-time.After(3 * time.Second):
		}
	}
}

// printWelcome prints the welcome text once the Platform has a URL and either
// a setup token exists or an admin has already been claimed.
func printWelcome(ctx context.Context, c client.Client, w io.Writer) (bool, error) {
	var p v1alpha1.Platform
	if err := c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if p.Status.URL == "" {
		return false, nil
	}
	token := ""
	if !p.Status.AdminClaimed {
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "setup-token"}, &s)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		token = string(s.Data["token"])
		if token == "" {
			return false, nil
		}
	}
	fmt.Fprintf(w, "\nswarmageddon is ready.\n\n")
	fmt.Fprintf(w, "  Open:            %s\n", p.Status.URL)
	if p.Status.CAFingerprint != "" {
		fmt.Fprintf(w, "  CA fingerprint:  %s\n", p.Status.CAFingerprint)
		fmt.Fprintf(w, "                   (check that your browser shows this before you type the token)\n")
	}
	if token != "" {
		fmt.Fprintf(w, "  Setup token:     %s\n", token)
	} else {
		fmt.Fprintf(w, "  An admin is already set up; sign in.\n")
	}
	fmt.Fprintf(w, "\nIf that address isn't reachable from your browser:\n")
	fmt.Fprintf(w, "  kubectl -n %s port-forward svc/control-center 8443:443\n", v1alpha1.SystemNamespace)
	fmt.Fprintf(w, "  then open https://localhost:8443/\n")
	return true, nil
}
