package k8splan

import (
	"strings"
	"testing"

	planapi "github.com/rancher/rancher/pkg/plan"
)

const (
	// progressChecksum is the checksum of the plan under reconciliation in these tests.
	progressChecksum = "checksum-a"
	// otherChecksum belongs to some other plan, for exercising the checkpoint's checksum scoping.
	otherChecksum = "checksum-b"
)

// progressData renders p into the Secret data map shape parsePlanCheckpoint and resolveResume read.
func progressData(p PlanCheckpoint) map[string][]byte {
	return map[string][]byte{planapi.PlanCheckpointKey: marshalPlanCheckpoint(p)}
}

// TestPlanProgressRoundTrip pins that a marshalled checkpoint parses back identically. It is also
// the assertion that a checkpoint written by a previous agent lifetime is indistinguishable from
// one this process wrote: there is deliberately no agent-instance field to round trip, and that
// absence is the design — an agent restarted while a plan is held must resume where it stopped.
func TestPlanProgressRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    PlanCheckpoint
	}{
		{
			name: "every field set",
			p: PlanCheckpoint{
				Checksum:    progressChecksum,
				Completed:   2,
				Total:       5,
				ResumeState: planapi.PlanStateInProgress,
				Paused:      true,
			},
		},
		{
			name: "only the required fields set",
			p: PlanCheckpoint{
				Checksum:  progressChecksum,
				Completed: 0,
				Total:     3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePlanCheckpoint(progressData(tt.p), progressChecksum)
			if got != tt.p {
				t.Errorf("round trip returned %+v, want %+v", got, tt.p)
			}
		})
	}
}

func TestParsePlanProgress(t *testing.T) {
	t.Parallel()

	stored := PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5, ResumeState: planapi.PlanStateInProgress, Paused: true}

	tests := []struct {
		name string
		data map[string][]byte
		want PlanCheckpoint
	}{
		{
			name: "checksum matches: returned verbatim",
			data: progressData(stored),
			want: stored,
		},
		{
			name: "checksum belongs to a different plan: zero value",
			data: progressData(PlanCheckpoint{Checksum: otherChecksum, Completed: 5, Total: 9, Paused: true}),
			want: PlanCheckpoint{},
		},
		{
			name: "key absent: zero value",
			data: map[string][]byte{AppliedChecksumKey: []byte(progressChecksum)},
			want: PlanCheckpoint{},
		},
		{
			name: "key present but empty (a cleared checkpoint): zero value",
			data: map[string][]byte{planapi.PlanCheckpointKey: {}},
			want: PlanCheckpoint{},
		},
		{
			name: "malformed JSON: zero value, no panic",
			data: map[string][]byte{planapi.PlanCheckpointKey: []byte("{not json")},
			want: PlanCheckpoint{},
		},
		{
			name: "well-formed JSON of the wrong shape: zero value, no panic",
			data: map[string][]byte{planapi.PlanCheckpointKey: []byte(`["a","b"]`)},
			want: PlanCheckpoint{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parsePlanCheckpoint(tt.data, progressChecksum); got != tt.want {
				t.Errorf("parsePlanProgress() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestMarshalPlanProgressPinsJSONTagNames guards the wire format: an operator reads this record
// with kubectl and a future Rancher may parse it, so a silent field rename would otherwise be
// invisible. The assertion is the exact encoding rather than a set of substrings, because
// json.Marshal emits fields in declaration order: matching the whole string pins the tag names,
// their order, and which field each tag is attached to. Substring checks would pass if two fields
// exchanged tags, and the round trip is symmetric under that swap.
func TestMarshalPlanProgressPinsJSONTagNames(t *testing.T) {
	t.Parallel()

	raw := string(marshalPlanCheckpoint(PlanCheckpoint{
		Checksum:              progressChecksum,
		Completed:             2,
		Total:                 5,
		ResumeState:           planapi.PlanStateInProgress,
		Paused:                true,
		TerminationIncomplete: true,
	}))

	want := `{"checksum":"checksum-a","completedInstructions":2,"totalInstructions":5,"resumeState":"in-progress","paused":true,"terminationIncomplete":true}`
	if raw != want {
		t.Errorf("marshalled checkpoint = %s, want %s", raw, want)
	}
}

// TestMarshalPlanProgressOmitsEmptyOptionalFields pins the omitempty behaviour: a plain progress
// report carries neither a resume state nor a paused flag.
func TestMarshalPlanProgressOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	raw := string(marshalPlanCheckpoint(PlanCheckpoint{Checksum: progressChecksum, Completed: 1, Total: 4}))

	for _, unwanted := range []string{`"resumeState"`, `"paused"`, `"terminationIncomplete"`} {
		if strings.Contains(raw, unwanted) {
			t.Errorf("expected marshalled checkpoint to omit %s, got %s", unwanted, raw)
		}
	}
	// The required fields are not omitempty and must survive their zero values.
	for _, want := range []string{`"checksum"`, `"completedInstructions"`, `"totalInstructions"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("expected marshalled checkpoint to contain %s, got %s", want, raw)
		}
	}
}

func TestResolveResume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state planapi.PlanState
		// progress is the checkpoint on the wire; nil means planapi.PlanCheckpointKey is absent.
		progress       *PlanCheckpoint
		wantState      planapi.PlanState
		wantResumeFrom int
	}{
		{
			name:           "checksum flow never has a checkpoint to resolve",
			state:          "",
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 4, Total: 6, Paused: true},
			wantState:      "",
			wantResumeFrom: 0,
		},
		{
			name:           "pending passes through",
			state:          planapi.PlanStatePending,
			wantState:      planapi.PlanStatePending,
			wantResumeFrom: 0,
		},
		{
			name:           "in-progress with no checkpoint keeps the crash-recovery contract: re-execute from 0",
			state:          planapi.PlanStateInProgress,
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			name:           "in-progress with a cancel report is not a suspension",
			state:          planapi.PlanStateInProgress,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			name:           "a live checkpoint resumes even when an external write moved plan-state out from under it",
			state:          planapi.PlanStateInProgress,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5, ResumeState: planapi.PlanStateInProgress, Paused: true},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 2,
		},
		{
			name:           "paused with no checkpoint (hand-edited Secret) resumes the state, not the position",
			state:          planapi.PlanStatePaused,
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			name:           "paused with an unsuspended checkpoint: Paused is the sole gate on Completed",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			name:           "the ordinary unpause resumes at the checkpoint",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5, ResumeState: planapi.PlanStateInProgress, Paused: true},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 2,
		},
		{
			// Defaulting here would re-execute a completed Day 2 operation in full.
			name:           "a pause on a plan running only periodic instructions restores succeeded",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 3, Total: 3, ResumeState: planapi.PlanStateSucceeded, Paused: true},
			wantState:      planapi.PlanStateSucceeded,
			wantResumeFrom: 3,
		},
		{
			name:           "an empty resume state falls back to in-progress",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 1, Total: 5, Paused: true},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 1,
		},
		{
			name:           "a checkpoint for a different plan must not position this one",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: otherChecksum, Completed: 5, Total: 8, ResumeState: planapi.PlanStateInProgress, Paused: true},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			// Resuming into "paused" is a silent permanent stall: decidePlanStateAction treats
			// every state it does not know as terminal, so the plan would never run again and
			// never leave paused, with no annotation left for an operator to remove. The agent
			// cannot write such a record; a hand-edited Secret can.
			name:           "a hand-edited resume state of paused is ignored rather than stalling the plan forever",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5, ResumeState: planapi.PlanStatePaused, Paused: true},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 2,
		},
		{
			name:           "a hand-edited resume state of paused is ignored without a suspended checkpoint too",
			state:          planapi.PlanStatePaused,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5, ResumeState: planapi.PlanStatePaused},
			wantState:      planapi.PlanStateInProgress,
			wantResumeFrom: 0,
		},
		{
			name:           "succeeded passes through",
			state:          planapi.PlanStateSucceeded,
			wantState:      planapi.PlanStateSucceeded,
			wantResumeFrom: 0,
		},
		{
			name:           "canceled is terminal and its report is never resumed from",
			state:          planapi.PlanStateCanceled,
			progress:       &PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 5},
			wantState:      planapi.PlanStateCanceled,
			wantResumeFrom: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := map[string][]byte{}
			if tt.progress != nil {
				data = progressData(*tt.progress)
			}
			gotState, gotResumeFrom := resolveResume(tt.state, data, progressChecksum)
			if gotState != tt.wantState {
				t.Errorf("resolveResume() state = %q, want %q", gotState, tt.wantState)
			}
			if gotResumeFrom != tt.wantResumeFrom {
				t.Errorf("resolveResume() resumeFrom = %d, want %d", gotResumeFrom, tt.wantResumeFrom)
			}
		})
	}
}

func TestOrDefault(t *testing.T) {
	t.Parallel()

	t.Run("plan states", func(t *testing.T) {
		t.Parallel()
		if got := orDefault(planapi.PlanStateSucceeded, planapi.PlanStateInProgress); got != planapi.PlanStateSucceeded {
			t.Errorf("orDefault() = %q, want %q", got, planapi.PlanStateSucceeded)
		}
		if got := orDefault(planapi.PlanState(""), planapi.PlanStateInProgress); got != planapi.PlanStateInProgress {
			t.Errorf("orDefault() = %q, want %q", got, planapi.PlanStateInProgress)
		}
	})

	t.Run("ints", func(t *testing.T) {
		t.Parallel()
		if got := orDefault(3, 7); got != 3 {
			t.Errorf("orDefault() = %d, want 3", got)
		}
		if got := orDefault(0, 7); got != 7 {
			t.Errorf("orDefault() = %d, want 7", got)
		}
	})
}
