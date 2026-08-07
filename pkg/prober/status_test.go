package prober

import (
	"testing"

	planapi "github.com/rancher/rancher/pkg/plan"
)

func TestResolveThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured int
		def        int
		want       int
	}{
		{name: "zero configured uses default", configured: 0, def: 3, want: 3},
		{name: "non-zero configured is used as-is", configured: 5, def: 3, want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := orDefault(tt.configured, tt.def); got != tt.want {
				t.Errorf("orDefault(%d, %d) = %d, want %d", tt.configured, tt.def, got, tt.want)
			}
		})
	}
}

func TestApplyProbeResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		initial          planapi.ProbeStatus
		succeeded        bool
		successThreshold int
		failureThreshold int
		want             planapi.ProbeStatus
	}{
		{
			name:             "success below threshold does not yet mark healthy",
			initial:          planapi.ProbeStatus{},
			succeeded:        true,
			successThreshold: 3,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: false, SuccessCount: 1, FailureCount: 0},
		},
		{
			name:             "success reaching threshold marks healthy",
			initial:          planapi.ProbeStatus{SuccessCount: 2},
			succeeded:        true,
			successThreshold: 3,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: true, SuccessCount: 3, FailureCount: 0},
		},
		{
			name:             "success at or above threshold does not keep incrementing",
			initial:          planapi.ProbeStatus{Healthy: true, SuccessCount: 3},
			succeeded:        true,
			successThreshold: 3,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: true, SuccessCount: 3, FailureCount: 0},
		},
		{
			name:             "success resets failure count",
			initial:          planapi.ProbeStatus{FailureCount: 2},
			succeeded:        true,
			successThreshold: 1,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: true, SuccessCount: 1, FailureCount: 0},
		},
		{
			name:             "failure below threshold does not yet mark unhealthy",
			initial:          planapi.ProbeStatus{Healthy: true},
			succeeded:        false,
			successThreshold: 1,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: true, SuccessCount: 0, FailureCount: 1},
		},
		{
			name:             "failure reaching threshold marks unhealthy",
			initial:          planapi.ProbeStatus{Healthy: true, FailureCount: 2},
			succeeded:        false,
			successThreshold: 1,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: false, SuccessCount: 0, FailureCount: 3},
		},
		{
			name:             "failure at or above threshold does not keep incrementing",
			initial:          planapi.ProbeStatus{FailureCount: 3},
			succeeded:        false,
			successThreshold: 1,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: false, SuccessCount: 0, FailureCount: 3},
		},
		{
			name:             "failure resets success count",
			initial:          planapi.ProbeStatus{SuccessCount: 2},
			succeeded:        false,
			successThreshold: 1,
			failureThreshold: 3,
			want:             planapi.ProbeStatus{Healthy: false, SuccessCount: 0, FailureCount: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status := tt.initial
			applyProbeResult(&status, tt.succeeded, tt.successThreshold, tt.failureThreshold)
			if status != tt.want {
				t.Errorf("applyProbeResult() = %+v, want %+v", status, tt.want)
			}
		})
	}
}
