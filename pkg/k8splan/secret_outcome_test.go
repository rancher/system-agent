package k8splan

import (
	"strings"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

func TestBuildSecretDataUpdates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	nowFormatted := now.Format(time.UnixDate)

	tests := []struct {
		name string
		in   applyOutcomeInput
		want map[string][]byte
	}{
		{
			name: "needsApplied succeeds: full success bookkeeping, plan-state set",
			in: applyOutcomeInput{
				Checksum:              "checksum-a",
				CurrentTime:           now,
				NeedsApplied:          true,
				OneTimeApplySucceeded: true,
				OneTimeOutput:         []byte("output-a"),
				PeriodicOutput:        []byte("periodic-a"),
				UsesPlanState:         true,
				PriorSuccessCount:     []byte("2"),
			},
			want: map[string][]byte{
				AppliedPeriodicOutputKey: []byte("periodic-a"),
				AppliedChecksumKey:       []byte("checksum-a"),
				AppliedOutputKey:         []byte("output-a"),
				FailureCountKey:          []byte("0"),
				FailedOutputKey:          {},
				FailedChecksumKey:        {},
				LastApplyTimeKey:         []byte(nowFormatted),
				SuccessCountKey:          []byte("3"),
				planapi.PlanStateKey:     []byte(planapi.PlanStateSucceeded),
			},
		},
		{
			name: "needsApplied fails: failure bookkeeping, plan-state set",
			in: applyOutcomeInput{
				Checksum:              "checksum-a",
				CurrentTime:           now,
				NeedsApplied:          true,
				OneTimeApplySucceeded: false,
				OneTimeOutput:         []byte("output-a"),
				PeriodicOutput:        []byte("periodic-a"),
				UsesPlanState:         true,
				PriorFailureCount:     []byte("1"),
			},
			want: map[string][]byte{
				AppliedPeriodicOutputKey: []byte("periodic-a"),
				FailedChecksumKey:        []byte("checksum-a"),
				FailureCountKey:          []byte("2"),
				FailedOutputKey:          []byte("output-a"),
				SuccessCountKey:          []byte("0"),
				LastApplyTimeKey:         []byte(nowFormatted),
				planapi.PlanStateKey:     []byte(planapi.PlanStateFailed),
			},
		},
		{
			name: "not needsApplied, not previously failed: steady-state success bookkeeping only, no plan-state or count churn",
			in: applyOutcomeInput{
				Checksum:              "checksum-a",
				CurrentTime:           now,
				NeedsApplied:          false,
				WasFailedPlan:         false,
				OneTimeApplySucceeded: false, // Applyinator always reports false when RunOneTimeInstructions is false
				OneTimeOutput:         []byte("output-a"),
				PeriodicOutput:        []byte("periodic-a"),
				UsesPlanState:         true,
			},
			want: map[string][]byte{
				AppliedPeriodicOutputKey: []byte("periodic-a"),
				AppliedChecksumKey:       []byte("checksum-a"),
				AppliedOutputKey:         []byte("output-a"),
				FailureCountKey:          []byte("0"),
				FailedOutputKey:          {},
				FailedChecksumKey:        {},
				// No LastApplyTimeKey, SuccessCountKey, or plan-state: gated on NeedsApplied.
			},
		},
		{
			name: "not needsApplied, was previously failed (cooldown/threshold): failure bookkeeping without count churn",
			in: applyOutcomeInput{
				Checksum:              "checksum-a",
				CurrentTime:           now,
				NeedsApplied:          false,
				WasFailedPlan:         true,
				OneTimeApplySucceeded: false,
				OneTimeOutput:         []byte("output-a"),
				PeriodicOutput:        []byte("periodic-a"),
				UsesPlanState:         false,
			},
			want: map[string][]byte{
				AppliedPeriodicOutputKey: []byte("periodic-a"),
				FailedChecksumKey:        []byte("checksum-a"),
				// No FailureCountKey/FailedOutputKey/SuccessCountKey/LastApplyTimeKey: all gated on NeedsApplied.
			},
		},
		{
			name: "checksum flow (UsesPlanState false) never writes plan-state key even on failure",
			in: applyOutcomeInput{
				Checksum:              "checksum-a",
				CurrentTime:           now,
				NeedsApplied:          true,
				OneTimeApplySucceeded: false,
				OneTimeOutput:         []byte("output-a"),
				PeriodicOutput:        []byte("periodic-a"),
				UsesPlanState:         false,
				PriorFailureCount:     []byte(""),
			},
			want: map[string][]byte{
				AppliedPeriodicOutputKey: []byte("periodic-a"),
				FailedChecksumKey:        []byte("checksum-a"),
				FailureCountKey:          []byte("1"),
				FailedOutputKey:          []byte("output-a"),
				SuccessCountKey:          []byte("0"),
				LastApplyTimeKey:         []byte(nowFormatted),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, logs := buildSecretDataUpdates(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("buildSecretDataUpdates() returned %d keys %v, want %d keys %v", len(got), keysOf(got), len(tt.want), keysOf(tt.want))
			}
			for k, wantV := range tt.want {
				gotV, ok := got[k]
				if !ok {
					t.Errorf("missing expected key %q", k)
					continue
				}
				if string(gotV) != string(wantV) {
					t.Errorf("key %q = %q, want %q", k, gotV, wantV)
				}
			}
			// Every outcome must record which branch it took; this is the line that tells an
			// operator whether the node stored a success or a failure.
			if len(logs) != 1 {
				t.Fatalf("expected exactly one outcome log, got %d: %+v", len(logs), logs)
			}
			wantLog := "writing an applied checksum value"
			if _, isFailure := got[FailedChecksumKey]; isFailure && len(got[FailedChecksumKey]) > 0 {
				wantLog = "either failed or was already failed"
			}
			if !strings.Contains(logs[0].Message, wantLog) {
				t.Errorf("expected outcome log to contain %q, got %q", wantLog, logs[0].Message)
			}
		})
	}
}

func keysOf(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
