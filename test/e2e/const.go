//go:build e2e

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

package e2e

import _ "embed"

var (
	//go:embed data/manifests/setup.yaml
	SetupManifest []byte

	//go:embed data/manifests/agent-config.yaml
	AgentConfigTemplate []byte

	//go:embed data/manifests/agent-daemonset.yaml
	AgentDaemonSetTemplate []byte

	//go:embed data/manifests/http-test-server.yaml
	HTTPTestServerManifestTemplate []byte
)

// Environment variable names for e2e configuration.
const (
	ImageTagVar            = "E2E_IMAGE_TAG"
	ImageNameVar           = "E2E_IMAGE_NAME"
	ClusterNameVar         = "E2E_KIND_CLUSTER_NAME"
	UseExistingClusterVar  = "USE_EXISTING_CLUSTER"
	SkipResourceCleanupVar = "SKIP_RESOURCE_CLEANUP"
	ArtifactsFolderVar     = "E2E_ARTIFACTS"
)
