package k8splan

import (
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
				AppliedPeriodicOutputKey:  []byte("periodic-a"),
				planapi.PlanCheckpointKey: {},
				AppliedChecksumKey:        []byte("checksum-a"),
				AppliedOutputKey:          []byte("output-a"),
				FailureCountKey:           []byte("0"),
				FailedOutputKey:           {},
				FailedChecksumKey:         {},
				LastApplyTimeKey:          []byte(nowFormatted),
				SuccessCountKey:           []byte("3"),
				planapi.PlanStateKey:      []byte(planapi.PlanStateSucceeded),
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
				AppliedPeriodicOutputKey:  []byte("periodic-a"),
				planapi.PlanCheckpointKey: {},
				FailedChecksumKey:         []byte("checksum-a"),
				FailureCountKey:           []byte("2"),
				FailedOutputKey:           []byte("output-a"),
				SuccessCountKey:           []byte("0"),
				LastApplyTimeKey:          []byte(nowFormatted),
				planapi.PlanStateKey:      []byte(planapi.PlanStateFailed),
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
				AppliedPeriodicOutputKey:  []byte("periodic-a"),
				planapi.PlanCheckpointKey: {},
				AppliedChecksumKey:        []byte("checksum-a"),
				AppliedOutputKey:          []byte("output-a"),
				FailureCountKey:           []byte("0"),
				FailedOutputKey:           {},
				FailedChecksumKey:         {},
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
			got := buildSecretDataUpdates(tt.in)
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
		})
	}
}

// TestBuildSecretDataUpdatesAlwaysClearsThePlanProgressCheckpoint pins Part 3's rule on both
// branches: every outcome this function produces is terminal for the apply, so a stale resume
// checkpoint must not survive into a later run.
//
// The clear must be an empty value and never a delete. updateSecret's conflict merge loop only
// carries over keys present in the in-hand copy, so a deleted key leaves the server's stale
// checkpoint in place and the clear is silently lost on a retry — see secretConflictMergeKeys and
// TestUpdateSecretConflictMergeCarriesAnEmptyClear.
func TestBuildSecretDataUpdatesAlwaysClearsThePlanProgressCheckpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   applyOutcomeInput
		// wantCleared is false for the checksum flow, which has never owned this key.
		wantCleared bool
	}{
		{
			name:        "plan-state flow, success path",
			in:          applyOutcomeInput{Checksum: "checksum-a", NeedsApplied: true, OneTimeApplySucceeded: true, UsesPlanState: true},
			wantCleared: true,
		},
		{
			name:        "plan-state flow, failure path",
			in:          applyOutcomeInput{Checksum: "checksum-a", NeedsApplied: true, OneTimeApplySucceeded: false, UsesPlanState: true},
			wantCleared: true,
		},
		{
			name: "checksum flow, success path: the key is never invented",
			in:   applyOutcomeInput{Checksum: "checksum-a", NeedsApplied: true, OneTimeApplySucceeded: true, UsesPlanState: false},
		},
		{
			name: "checksum flow, failure path: the key is never invented",
			in:   applyOutcomeInput{Checksum: "checksum-a", NeedsApplied: true, OneTimeApplySucceeded: false, UsesPlanState: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildSecretDataUpdates(tt.in)
			value, ok := got[planapi.PlanCheckpointKey]
			if !tt.wantCleared {
				if ok {
					t.Errorf("expected the checksum flow never to write %q, got %q", planapi.PlanCheckpointKey, value)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %q to be present as an empty-value clear; a delete does not survive a conflict retry", planapi.PlanCheckpointKey)
			}
			if len(value) != 0 {
				t.Errorf("expected %q to be cleared to an empty value, got %q", planapi.PlanCheckpointKey, value)
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
