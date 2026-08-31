package prober

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

func TestDoProbeMarksHealthyAfterSuccessThreshold(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	probe := planapi.Probe{
		Name:             "test-probe",
		SuccessThreshold: 2,
		TimeoutSeconds:   5,
		HTTPGetAction:    planapi.HTTPGetAction{URL: server.URL, Insecure: true},
	}
	var status planapi.ProbeStatus

	for i := 0; i < 2; i++ {
		if err := DoProbe(probe, &status, false); err != nil {
			t.Fatalf("DoProbe returned error on attempt %d: %v", i, err)
		}
	}

	if !status.Healthy {
		t.Errorf("expected probe to be healthy after reaching success threshold, got %+v", status)
	}
	if status.SuccessCount != 2 {
		t.Errorf("expected success count 2, got %d", status.SuccessCount)
	}
}

func TestDoProbeMarksUnhealthyAfterFailureThreshold(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	probe := planapi.Probe{
		Name:             "test-probe",
		FailureThreshold: 2,
		TimeoutSeconds:   5,
		HTTPGetAction:    planapi.HTTPGetAction{URL: server.URL, Insecure: true},
	}
	status := planapi.ProbeStatus{Healthy: true}

	for i := 0; i < 2; i++ {
		if err := DoProbe(probe, &status, false); err != nil {
			t.Fatalf("DoProbe returned error on attempt %d: %v", i, err)
		}
	}

	if status.Healthy {
		t.Errorf("expected probe to be unhealthy after reaching failure threshold, got %+v", status)
	}
	if status.FailureCount != 2 {
		t.Errorf("expected failure count 2, got %d", status.FailureCount)
	}
}

func TestDoProbeInvalidURLReturnsError(t *testing.T) {
	t.Parallel()

	probe := planapi.Probe{
		Name:          "test-probe",
		HTTPGetAction: planapi.HTTPGetAction{URL: "://not-a-valid-url", Insecure: true},
	}
	var status planapi.ProbeStatus

	if err := DoProbe(probe, &status, false); err == nil {
		t.Fatal("expected an error for an invalid probe URL, got nil")
	}
}

func TestDoProbeSleepsForInitialDelayOnlyWhenInitial(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	probe := planapi.Probe{
		Name:                "test-probe",
		InitialDelaySeconds: 0, // kept at zero so this test doesn't actually sleep
		TimeoutSeconds:      5,
		HTTPGetAction:       planapi.HTTPGetAction{URL: server.URL, Insecure: true},
	}
	var status planapi.ProbeStatus

	if err := DoProbe(probe, &status, true); err != nil {
		t.Fatalf("DoProbe returned error: %v", err)
	}
	if status.SuccessCount != 1 {
		t.Errorf("expected the initial probe to still record success, got %+v", status)
	}
}

func TestDoProbesUpdatesStatusesForAllProbesConcurrently(t *testing.T) {
	t.Parallel()

	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okServer.Close()

	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failServer.Close()

	probes := map[string]planapi.Probe{
		"healthy-probe": {SuccessThreshold: 1, TimeoutSeconds: 5, HTTPGetAction: planapi.HTTPGetAction{URL: okServer.URL, Insecure: true}},
		"failing-probe": {FailureThreshold: 1, TimeoutSeconds: 5, HTTPGetAction: planapi.HTTPGetAction{URL: failServer.URL, Insecure: true}},
		"bad-url-probe": {HTTPGetAction: planapi.HTTPGetAction{URL: "://not-a-valid-url", Insecure: true}},
	}
	statuses := map[string]planapi.ProbeStatus{}

	DoProbes(probes, statuses, false)

	if !statuses["healthy-probe"].Healthy {
		t.Errorf("expected healthy-probe to be healthy, got %+v", statuses["healthy-probe"])
	}
	if statuses["failing-probe"].Healthy {
		t.Errorf("expected failing-probe to be unhealthy, got %+v", statuses["failing-probe"])
	}
	if _, ok := statuses["bad-url-probe"]; !ok {
		t.Error("expected bad-url-probe to still get a status entry despite DoProbe returning an error")
	}
}

func TestGetSystemCertPoolReturnsAPool(t *testing.T) {
	t.Parallel()

	pool, err := GetSystemCertPool("test-probe")
	if err != nil {
		t.Fatalf("GetSystemCertPool returned error: %v", err)
	}
	if pool == nil {
		t.Fatal("expected a non-nil cert pool")
	}
}

// writeTempPEM writes content to a temp file and returns its path.
func writeTempPEM(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTLSServer starts an HTTPS test server and returns it alongside a PEM file containing its CA.
func newTLSServer(t *testing.T, status int) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	return server, writeTempPEM(t, "ca.pem", caPEM)
}

// These cases exercise the non-Insecure branch of DoProbe (the client-cert load, system cert pool
// wiring, and CA-cert append), which every other test in this file bypasses via Insecure: true.
func TestDoProbeTLS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// action is built per-test because the CA path depends on the server.
		action      func(t *testing.T, serverURL, caPath string) planapi.HTTPGetAction
		serverCode  int
		wantHealthy bool
	}{
		{
			name: "valid CA cert verifies the server",
			action: func(_ *testing.T, serverURL, caPath string) planapi.HTTPGetAction {
				return planapi.HTTPGetAction{URL: serverURL, CACert: caPath}
			},
			serverCode:  http.StatusOK,
			wantHealthy: true,
		},
		{
			name: "unreadable CA cert falls back to the system pool and fails verification",
			action: func(t *testing.T, serverURL, _ string) planapi.HTTPGetAction {
				return planapi.HTTPGetAction{URL: serverURL, CACert: filepath.Join(t.TempDir(), "missing-ca.pem")}
			},
			serverCode:  http.StatusOK,
			wantHealthy: false,
		},
		{
			name: "malformed CA cert falls back to the system pool and fails verification",
			action: func(t *testing.T, serverURL, _ string) planapi.HTTPGetAction {
				return planapi.HTTPGetAction{URL: serverURL, CACert: writeTempPEM(t, "bad-ca.pem", []byte("not a pem"))}
			},
			serverCode:  http.StatusOK,
			wantHealthy: false,
		},
		{
			// Regression guard for the zero-value tls.Certificate bug: an unloadable client cert
			// must be skipped, leaving the connection otherwise intact rather than poisoning the
			// TLS config.
			name: "unloadable client cert is skipped, probe still completes",
			action: func(t *testing.T, serverURL, caPath string) planapi.HTTPGetAction {
				dir := t.TempDir()
				return planapi.HTTPGetAction{
					URL:        serverURL,
					CACert:     caPath,
					ClientCert: filepath.Join(dir, "missing-cert.pem"),
					ClientKey:  filepath.Join(dir, "missing-key.pem"),
				}
			},
			serverCode:  http.StatusOK,
			wantHealthy: true,
		},
		{
			name: "valid CA cert but failing server marks unhealthy",
			action: func(_ *testing.T, serverURL, caPath string) planapi.HTTPGetAction {
				return planapi.HTTPGetAction{URL: serverURL, CACert: caPath}
			},
			serverCode:  http.StatusInternalServerError,
			wantHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, caPath := newTLSServer(t, tt.serverCode)
			probe := planapi.Probe{
				Name:             "tls-probe",
				SuccessThreshold: 1,
				FailureThreshold: 1,
				TimeoutSeconds:   5,
				HTTPGetAction:    tt.action(t, server.URL, caPath),
			}

			var status planapi.ProbeStatus
			// A TLS handshake failure surfaces as probe.Failure with a nil error, not as an error
			// return, so DoProbe should not error in any of these cases.
			if err := DoProbe(probe, &status, false); err != nil {
				t.Fatalf("DoProbe returned error: %v", err)
			}
			if status.Healthy != tt.wantHealthy {
				t.Errorf("expected Healthy=%v, got %+v", tt.wantHealthy, status)
			}
		})
	}
}

func TestDoProbeDefaultsTimeoutWhenUnset(t *testing.T) {
	t.Parallel()

	// TimeoutSeconds: 0 must not mean "no timeout". A server that never responds has to be given
	// up on, or the probe goroutine blocks DoProbes' WaitGroup forever.
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer server.Close()
	defer close(blocked)

	probe := planapi.Probe{
		Name:             "no-timeout-probe",
		FailureThreshold: 1,
		HTTPGetAction:    planapi.HTTPGetAction{URL: server.URL, Insecure: true},
	}

	done := make(chan error, 1)
	var status planapi.ProbeStatus
	go func() { done <- DoProbe(probe, &status, false) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DoProbe did not return; TimeoutSeconds=0 was treated as an unlimited timeout")
	}
}
