package applyinator

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"testing"
)

func Must[T any](t *testing.T, f func() (T, error)) T {
	out, err := f()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func Bind[T any, U any](t T, f func(T) (U, error)) func() (U, error) {
	return func() (u U, err error) {
		return f(t)
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name        string
		input       ApplyInput
		expectError bool
		output      ApplyOutput
	}{
		{
			name:        "no instructions",
			input:       ApplyInput{},
			expectError: false,
			output: ApplyOutput{
				OneTimeOutput:          nil,
				OneTimeApplySucceeded:  false,
				PeriodicOutput:         Must(t, Bind([]byte("{}"), gzipByteSlice)),
				PeriodicApplySucceeded: true,
			},
		},
		{
			name: "simple instruction - apply not needed",
			input: ApplyInput{
				CalculatedPlan: CalculatedPlan{
					Plan: Plan{
						OneTimeInstructions: []OneTimeInstruction{
							{
								CommonInstruction: CommonInstruction{
									Name:    "echo-command",
									Command: "echo test",
								},
							},
						},
					},
				},
			},
			expectError: false,
			output: ApplyOutput{
				OneTimeOutput:          nil,
				OneTimeApplySucceeded:  false,
				PeriodicOutput:         Must(t, Bind([]byte("{}"), gzipByteSlice)),
				PeriodicApplySucceeded: true,
			},
		},
		{
			name: "simple instruction",
			input: ApplyInput{
				CalculatedPlan: CalculatedPlan{
					Plan: Plan{
						OneTimeInstructions: []OneTimeInstruction{
							{
								CommonInstruction: CommonInstruction{
									Name:    "echo-command",
									Command: "echo",
									Args:    []string{"test"},
								},
							},
						},
					},
				},
				RunOneTimeInstructions: true,
			},
			expectError: false,
			output: ApplyOutput{
				OneTimeOutput:          Must(t, Bind([]byte("{}"), gzipByteSlice)),
				OneTimeApplySucceeded:  true,
				PeriodicOutput:         Must(t, Bind([]byte("{}"), gzipByteSlice)),
				PeriodicApplySucceeded: true,
			},
		},
		{
			name: "simple instruction - save output",
			input: ApplyInput{
				CalculatedPlan: CalculatedPlan{
					Plan: Plan{
						OneTimeInstructions: []OneTimeInstruction{
							{
								CommonInstruction: CommonInstruction{
									Name:    "echo-command",
									Command: "echo",
									Args:    []string{"test"},
								},
								SaveOutput: true,
							},
						},
					},
				},
				RunOneTimeInstructions: true,
			},
			expectError: false,
			output: ApplyOutput{
				OneTimeOutput:          Must(t, Bind([]byte(fmt.Sprintf(`{"echo-command":"%s"}`, base64.StdEncoding.EncodeToString([]byte("test\n")))), gzipByteSlice)),
				OneTimeApplySucceeded:  true,
				PeriodicOutput:         Must(t, Bind([]byte("{}"), gzipByteSlice)),
				PeriodicApplySucceeded: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)

			workDir := filepath.Join(tmpDir, "work")
			assert.NoError(t, os.Mkdir(workDir, 0777))

			appliedPlanDir := filepath.Join(tmpDir, "applied")
			assert.NoError(t, os.Mkdir(appliedPlanDir, 0777))

			interlockDir := filepath.Join(tmpDir, "interlock")
			assert.NoError(t, os.Mkdir(interlockDir, 0777))

			a := NewApplyinator(workDir, false, appliedPlanDir, interlockDir, image.NewUtility("", "", "", ""))
			output, err := a.Apply(context.Background(), tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.output, output)
			}
		})
	}
}
