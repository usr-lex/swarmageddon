package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestNtfyBearerToken(t *testing.T) {
	var auth, path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		if auth != "Bearer tk_secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	dir := &fakeDir{
		settings: v1alpha1.NotificationSettings{Channels: []v1alpha1.NotificationChannel{{Kind: "ntfy", Topic: "acme"}}},
		secrets:  map[string]map[string][]byte{NtfySecretName: {"token": []byte("tk_secret\n")}},
	}
	var sendErr error
	d := &Dispatcher{Dir: dir, NtfyURL: s.URL, Sent: func(_ string, _ Event, err error) { sendErr = err }}
	d.Raise(context.Background(), Event{Kind: v1alpha1.EventCIRed, Workspace: "acme", Title: "t", Body: "b", Fingerprint: "1"})
	if sendErr != nil || auth != "Bearer tk_secret" || path != "/acme" {
		t.Fatalf("err=%v auth=%q path=%q", sendErr, auth, path)
	}
	// Without a token ntfy refuses (deny-all), and the failure is reported.
	dir.secrets = nil
	d.Raise(context.Background(), Event{Kind: v1alpha1.EventCIRed, Workspace: "acme", Title: "t", Body: "b", Fingerprint: "2"})
	if sendErr == nil || auth != "" {
		t.Fatalf("anonymous publish: err=%v auth=%q", sendErr, auth)
	}
}
