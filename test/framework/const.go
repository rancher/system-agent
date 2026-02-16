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

const (
	// E2ENamespace is the namespace used for e2e test resources.
	E2ENamespace = "system-agent-e2e"

	// AgentLabel is the label used to identify system-agent pods.
	AgentLabel = "app=system-agent"

	// AgentContainerName is the name of the system-agent container in the DaemonSet.
	AgentContainerName = "system-agent"

	// PlanSecretName is the default name for the plan Secret in remote mode.
	PlanSecretName = "agent-plan"

	// ShortTestLabel is used to filter tests for PR gate (fast tests).
	ShortTestLabel = "short"

	// FullTestLabel is used for nightly (all tests).
	FullTestLabel = "full"

	// DefaultPollInterval is the default interval for polling conditions.
	DefaultPollInterval = "2s"

	// DefaultTimeout is the default timeout for waiting on conditions.
	DefaultTimeout = "60s"
)
