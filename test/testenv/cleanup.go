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

package testenv

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
)

// CleanupTestClusterInput represents the input parameters for cleaning up the test cluster.
type CleanupTestClusterInput struct {
	SetupTestClusterResult
}

// CleanupTestCluster tears down the test cluster by disposing of the bootstrap cluster provider.
func CleanupTestCluster(ctx context.Context, input CleanupTestClusterInput) {
	if input.BootstrapClusterProvider != nil {
		By("Disposing of bootstrap cluster")
		input.BootstrapClusterProvider.Dispose(ctx)
	}
}
