package prober

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
