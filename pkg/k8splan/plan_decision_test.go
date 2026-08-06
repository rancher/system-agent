package k8splan

import (
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

func TestParseLastApplyTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		data map[string][]byte
		want time.Time
	}{
		{
			name: "key absent defaults to current time",
			data: map[string][]byte{},
			want: now,
		},
		{
			name: "valid UnixDate string is parsed",
			data: map[string][]byte{LastApplyTimeKey: []byte(now.Add(-time.Hour).Format(time.UnixDate))},
			want: now.Add(-time.Hour),
		},
		{
			name: "unparsable value falls back to current time",
			data: map[string][]byte{LastApplyTimeKey: []byte("not-a-time")},
			want: now,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseLastApplyTime(tt.data, now)
			if !got.Equal(tt.want) {
				t.Errorf("parseLastApplyTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseProbePeriodOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    map[string][]byte
		current time.Duration
		want    time.Duration
	}{
		{name: "key absent keeps current", data: map[string][]byte{}, current: 5 * time.Second, want: 5 * time.Second},
		{name: "valid override in seconds", data: map[string][]byte{ProbePeriodKey: []byte("10")}, current: 5 * time.Second, want: 10 * time.Second},
		{name: "unparsable override keeps current", data: map[string][]byte{ProbePeriodKey: []byte("garbage")}, current: 5 * time.Second, want: 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseProbePeriodOverride(tt.data, tt.current); got != tt.want {
				t.Errorf("parseProbePeriodOverride() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseProbeStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data map[string][]byte
		want map[string]planapi.ProbeStatus
	}{
		{name: "key absent yields empty map", data: map[string][]byte{}, want: map[string]planapi.ProbeStatus{}},
		{
			name: "valid JSON is parsed",
			data: map[string][]byte{ProbeStatusesKey: []byte(`{"kube-api":{"healthy":true,"successCount":3}}`)},
			want: map[string]planapi.ProbeStatus{"kube-api": {Healthy: true, SuccessCount: 3}},
		},
		{name: "invalid JSON yields empty map", data: map[string][]byte{ProbeStatusesKey: []byte("not-json")}, want: map[string]planapi.ProbeStatus{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseProbeStatuses(tt.data)
			if len(got) != len(tt.want) {
				t.Fatalf("parseProbeStatuses() = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseProbeStatuses()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestParseFailureCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		data             map[string][]byte
		wantFailureCount int
		wantPlanAttempt  int
	}{
		{name: "key absent", data: map[string][]byte{}, wantFailureCount: 0, wantPlanAttempt: 1},
		{name: "empty value", data: map[string][]byte{FailureCountKey: []byte("")}, wantFailureCount: 0, wantPlanAttempt: 1},
		{name: "valid count", data: map[string][]byte{FailureCountKey: []byte("3")}, wantFailureCount: 3, wantPlanAttempt: 4},
		{name: "unparsable count defaults to zero but still increments attempt", data: map[string][]byte{FailureCountKey: []byte("garbage")}, wantFailureCount: 0, wantPlanAttempt: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotFailureCount, gotPlanAttempt := parseFailureCount(tt.data)
			if gotFailureCount != tt.wantFailureCount || gotPlanAttempt != tt.wantPlanAttempt {
				t.Errorf("parseFailureCount() = (%d, %d), want (%d, %d)", gotFailureCount, gotPlanAttempt, tt.wantFailureCount, tt.wantPlanAttempt)
			}
		})
	}
}

func TestDecidePlanStateAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state planapi.PlanState
		want  planStateResult
	}{
		{name: "pending applies and resets attempt", state: planapi.PlanStatePending, want: planStateResult{NeedsApplied: true, ResetPlanAttempt: true}},
		{name: "in-progress re-executes without resetting attempt", state: planapi.PlanStateInProgress, want: planStateResult{NeedsApplied: true, ResetPlanAttempt: false}},
		{name: "succeeded is terminal", state: planapi.PlanStateSucceeded, want: planStateResult{NeedsApplied: false, ResetPlanAttempt: false}},
		{name: "failed is terminal", state: planapi.PlanStateFailed, want: planStateResult{NeedsApplied: false, ResetPlanAttempt: false}},
		{name: "cancelled is terminal", state: planapi.PlanStateCancelled, want: planStateResult{NeedsApplied: false, ResetPlanAttempt: false}},
		{name: "unknown state is treated as terminal", state: planapi.PlanState("some-future-state"), want: planStateResult{NeedsApplied: false, ResetPlanAttempt: false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decidePlanStateAction(tt.state); got != tt.want {
				t.Errorf("decidePlanStateAction(%q) = %+v, want %+v", tt.state, got, tt.want)
			}
		})
	}
}

func TestSelectExistingOutput(t *testing.T) {
	t.Parallel()

	data := map[string][]byte{
		AppliedOutputKey: []byte("applied-output"),
		FailedOutputKey:  []byte("failed-output"),
	}

	tests := []struct {
		name          string
		data          map[string][]byte
		wasFailedPlan bool
		want          string
	}{
		{name: "not failed uses applied output", data: data, wasFailedPlan: false, want: "applied-output"},
		{name: "failed uses failed output", data: data, wasFailedPlan: true, want: "failed-output"},
		{name: "missing applied output defaults to empty", data: map[string][]byte{}, wasFailedPlan: false, want: ""},
		{name: "missing failed output defaults to empty", data: map[string][]byte{}, wasFailedPlan: true, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(selectExistingOutput(tt.data, tt.wasFailedPlan)); got != tt.want {
				t.Errorf("selectExistingOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}
