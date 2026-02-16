# System Agent E2E Tests

## Overview

The e2e test suite validates system-agent functionality in a real Kubernetes environment. Tests create a Kind cluster, deploy the system-agent as a DaemonSet, and verify various agent capabilities.

## Running Tests

From the repository root:

```bash
# Run short tests
make test-e2e

# Run long tests only
GINKGO_LABEL_FILTER="long" make test-e2e

# Run with custom image
E2E_IMAGE_TAG=dev E2E_IMAGE_NAME=myregistry/system-agent make test-e2e

# Keep cluster after tests for debugging
SKIP_RESOURCE_CLEANUP=true make test-e2e
```

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `E2E_IMAGE_NAME` | System-agent image name | `rancher/system-agent` |
| `E2E_IMAGE_TAG` | Image tag to test | `e2e-test` |
| `E2E_KIND_CLUSTER_NAME` | Kind cluster name | `system-agent-e2e` |
| `SKIP_RESOURCE_CLEANUP` | Preserve cluster after tests | `false` |
| `GINKGO_LABEL_FILTER` | Ginkgo label filter | `short` |
| `GINKGO_NODES` | Parallel test nodes | `1` |
| `GINKGO_TIMEOUT` | Overall test timeout | `30m` |

### Test Labels

Tests are categorized with labels for selective execution:

- `short`: Quick tests for CI
- `long`: Extended tests that take longer to run(nightly)
