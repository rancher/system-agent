// Copyright © 2025 SUSE LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package framework

import (
	"encoding/base64"
	"encoding/json"

	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/prober"
)

// PlanBuilder provides a fluent API for constructing applyinator plans.
type PlanBuilder struct {
	plan applyinator.Plan
}

// NewPlan creates a new PlanBuilder.
func NewPlan() *PlanBuilder {
	return &PlanBuilder{
		plan: applyinator.Plan{
			Probes: make(map[string]prober.Probe),
		},
	}
}

// WithFile adds a file creation operation to the plan.
// The content is provided as plain text and will be base64-encoded automatically.
func (b *PlanBuilder) WithFile(path, content, permissions string) *PlanBuilder {
	b.plan.Files = append(b.plan.Files, applyinator.File{
		Path:        path,
		Content:     base64.StdEncoding.EncodeToString([]byte(content)),
		Permissions: permissions,
	})
	return b
}

// WithDirectory adds a directory creation operation to the plan.
func (b *PlanBuilder) WithDirectory(path string) *PlanBuilder {
	b.plan.Files = append(b.plan.Files, applyinator.File{
		Path:      path,
		Directory: true,
	})
	return b
}

// WithDeleteFile adds a file deletion operation to the plan.
func (b *PlanBuilder) WithDeleteFile(path string) *PlanBuilder {
	b.plan.Files = append(b.plan.Files, applyinator.File{
		Path:   path,
		Action: "delete",
	})
	return b
}

// WithInstruction adds a one-time instruction to the plan.
func (b *PlanBuilder) WithInstruction(name, command string, args []string, saveOutput bool) *PlanBuilder {
	b.plan.OneTimeInstructions = append(b.plan.OneTimeInstructions, applyinator.OneTimeInstruction{
		CommonInstruction: applyinator.CommonInstruction{
			Name:    name,
			Command: command,
			Args:    args,
		},
		SaveOutput: saveOutput,
	})
	return b
}

// WithInstructionEnv adds a one-time instruction with environment variables.
func (b *PlanBuilder) WithInstructionEnv(name, command string, args, env []string, saveOutput bool) *PlanBuilder {
	b.plan.OneTimeInstructions = append(b.plan.OneTimeInstructions, applyinator.OneTimeInstruction{
		CommonInstruction: applyinator.CommonInstruction{
			Name:    name,
			Command: command,
			Args:    args,
			Env:     env,
		},
		SaveOutput: saveOutput,
	})
	return b
}

// WithPeriodicInstruction adds a periodic instruction to the plan.
func (b *PlanBuilder) WithPeriodicInstruction(name, command string, args []string, periodSeconds int) *PlanBuilder {
	b.plan.PeriodicInstructions = append(b.plan.PeriodicInstructions, applyinator.PeriodicInstruction{
		CommonInstruction: applyinator.CommonInstruction{
			Name:    name,
			Command: command,
			Args:    args,
		},
		PeriodSeconds: periodSeconds,
	})
	return b
}

// WithProbe adds an HTTP probe to the plan.
func (b *PlanBuilder) WithProbe(name, url string, insecure bool) *PlanBuilder {
	b.plan.Probes[name] = prober.Probe{
		Name:             name,
		HTTPGetAction:    prober.HTTPGetAction{URL: url, Insecure: insecure},
		TimeoutSeconds:   5,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	return b
}

// Build marshals the plan to JSON bytes.
func (b *PlanBuilder) Build() []byte {
	data, err := json.Marshal(b.plan)
	Expect(err).NotTo(HaveOccurred(), "failed to marshal plan")
	return data
}
