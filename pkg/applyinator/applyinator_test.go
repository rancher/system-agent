//go:build !arm64

package applyinator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/system-agent/pkg/image"
	"github.com/stretchr/testify/assert"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name                           string
		input                          ApplyInput
		expectError                    bool
		expectedOneTimeOutput          map[string][]byte
		expectedOneTimeApplySucceeded  bool
		expectedPeriodicOutput         map[string]PeriodicInstructionOutput
		expectedPeriodicApplySucceeded bool
	}{
		{
			name:                           "no instructions",
			input:                          ApplyInput{},
			expectError:                    false,
			expectedOneTimeOutput:          nil,
			expectedOneTimeApplySucceeded:  false,
			expectedPeriodicOutput:         map[string]PeriodicInstructionOutput{},
			expectedPeriodicApplySucceeded: true,
		},
		{
			name: "onetime - apply not needed",
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
			expectError:                    false,
			expectedOneTimeOutput:          nil,
			expectedOneTimeApplySucceeded:  false,
			expectedPeriodicOutput:         map[string]PeriodicInstructionOutput{},
			expectedPeriodicApplySucceeded: true,
		},
		{
			name: "onetime instruction",
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
			expectError:                    false,
			expectedOneTimeOutput:          map[string][]byte{},
			expectedOneTimeApplySucceeded:  true,
			expectedPeriodicOutput:         map[string]PeriodicInstructionOutput{},
			expectedPeriodicApplySucceeded: true,
		},
		{
			name: "onetime instruction - save output",
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
			expectedOneTimeOutput: map[string][]byte{
				"echo-command": []byte("test\n"),
			},
			expectedOneTimeApplySucceeded:  true,
			expectedPeriodicOutput:         map[string]PeriodicInstructionOutput{},
			expectedPeriodicApplySucceeded: true,
		},
		{
			name: "onetime instruction - failed",
			input: ApplyInput{
				CalculatedPlan: CalculatedPlan{
					Plan: Plan{
						OneTimeInstructions: []OneTimeInstruction{
							{
								CommonInstruction: CommonInstruction{
									Name:    "exit-command",
									Command: "sh",
									Args:    []string{"-c", "exit 1;"},
								},
							},
						},
					},
				},
				RunOneTimeInstructions: true,
			},
			expectError:                    false,
			expectedOneTimeOutput:          map[string][]byte{},
			expectedOneTimeApplySucceeded:  false,
			expectedPeriodicOutput:         map[string]PeriodicInstructionOutput{},
			expectedPeriodicApplySucceeded: true,
		},
		{
			name: "periodic instruction",
			input: ApplyInput{
				CalculatedPlan: CalculatedPlan{
					Plan: Plan{
						PeriodicInstructions: []PeriodicInstruction{
							{
								CommonInstruction: CommonInstruction{
									Name:    "echo-command",
									Command: "echo",
									Args:    []string{"test"},
								},
								PeriodSeconds: 10,
							},
						},
					},
				},
			},
			expectError:                   false,
			expectedOneTimeOutput:         nil,
			expectedOneTimeApplySucceeded: false,
			expectedPeriodicOutput: map[string]PeriodicInstructionOutput{
				"echo-command": {
					Name:     "echo-command",
					Stdout:   []byte("test\n"),
					Stderr:   []byte(""),
					ExitCode: 0,
					Failures: 0,
				},
			},
			expectedPeriodicApplySucceeded: true,
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

				assert.Equal(t, tt.expectedOneTimeApplySucceeded, output.OneTimeApplySucceeded)
				if tt.expectedOneTimeOutput == nil {
					assert.Nil(t, output.OneTimeOutput)
				} else {
					buffer := bytes.NewBuffer(output.OneTimeOutput)
					gzReader, err := gzip.NewReader(buffer)
					assert.NoError(t, err)

					var objectBuffer bytes.Buffer
					_, err = io.Copy(&objectBuffer, gzReader)
					assert.NoError(t, err)

					ungzippedOutput := map[string][]byte{}
					err = json.Unmarshal(objectBuffer.Bytes(), &ungzippedOutput)
					assert.NoError(t, err)

					assert.Len(t, ungzippedOutput, len(tt.expectedOneTimeOutput))
					assert.Equal(t, tt.expectedOneTimeOutput, ungzippedOutput)
				}

				assert.Equal(t, tt.expectedPeriodicApplySucceeded, output.PeriodicApplySucceeded)

				buffer := bytes.NewBuffer(output.PeriodicOutput)
				gzReader, err := gzip.NewReader(buffer)
				assert.NoError(t, err)

				var objectBuffer bytes.Buffer
				_, err = io.Copy(&objectBuffer, gzReader)
				assert.NoError(t, err)

				ungzippedOutput := map[string]PeriodicInstructionOutput{}
				err = json.Unmarshal(objectBuffer.Bytes(), &ungzippedOutput)
				assert.NoError(t, err)

				assert.Len(t, ungzippedOutput, len(tt.expectedPeriodicOutput))
				if len(tt.expectedPeriodicOutput) == 0 {
					assert.Equal(t, "{}", string(objectBuffer.Bytes()))
				} else {
					for k1, v1 := range tt.expectedPeriodicOutput {
						v2, ok := ungzippedOutput[k1]
						assert.True(t, ok, "output does not contain expected key")

						assert.Equal(t, v1.Name, v2.Name)
						assert.Equal(t, v1.Stdout, v2.Stdout)
						assert.Equal(t, v1.Stderr, v2.Stderr)
						assert.Equal(t, v1.ExitCode, v2.ExitCode)
						if v1.ExitCode == 0 {
							assert.NotEmpty(t, v2.LastSuccessfulRunTime)
						}
						assert.Equal(t, v1.Failures, v2.Failures)
						if v1.Failures != 0 {
							assert.NotEmpty(t, v2.LastFailedRunTime)
						}
					}
				}
			}
		})
	}
}
