# System Agent E2E Tests

## Overview

The e2e test suite validates system-agent functionality in a real Kubernetes environment. Tests create a Kind cluster, deploy the system-agent as a DaemonSet, and verify various agent capabilities against a real plan Secret — no mocked filesystem or exec calls.

**Scope note:** every spec here exercises **remote mode only** (a plan delivered via a watched Kubernetes Secret, `pkg/k8splan`). Local, file-based plan mode (`pkg/localplan`, `.plan` files on disk) has no e2e coverage today — see "Known Coverage Gaps" below.

See also: [integration/README.md](integration/README.md) for the v2prov-based integration test suite.

### Directory layout

```
test/
├── e2e/
│   ├── suites/remote-plan/
│   ├── data/manifests/
│   ├── const.go
│   └── helpers.go
├── framework/
│   ├── const.go
│   ├── cluster.go
│   ├── plan.go
│   ├── secret.go
│   ├── decode.go
│   └── template.go
├── testenv/
│   ├── setup.go
│   └── cleanup.go
└── integration/
```

- `e2e/suites/remote-plan/`: the single Ginkgo suite (`TestRemotePlan`) with all specs.
- `e2e/data/manifests/`: embedded YAML for namespace/RBAC, agent config, DaemonSet, and HTTP test server.
- `e2e/const.go`: embedded manifest and env-var constants.
- `e2e/helpers.go`: `E2EConfig` loading, scheme setup, and shared parallel-proc state.
- `framework/`: reusable spec helpers (no Ginkgo specs live here).
- `framework/const.go`: namespace/label/timeout constants (`ShortTestLabel`, `LongTestLabel`).
- `framework/cluster.go`: kubectl wrappers (apply/wait/exec/logs/server lifecycle).
- `framework/plan.go`: fluent `PlanBuilder` for plan JSON.
- `framework/secret.go`: plan Secret CRUD and polling helpers.
- `framework/decode.go`: gzip+base64+JSON decoding for output fields.
- `framework/template.go`: `${VAR}` template rendering for embedded manifests.
- `testenv/`: Kind cluster and agent lifecycle helpers used by `suite_test.go`.
- `testenv/setup.go`: cluster setup and remote-agent deployment.
- `testenv/cleanup.go`: Kind cluster teardown.
- `integration/`: separate non-Ginkgo suite (see `integration/README.md`).

`test/` is a **separate Go module** (`test/go.mod`, with `replace github.com/rancher/system-agent => ../`). This is deliberate: it keeps the Ginkgo and Kind dependency trees out of the main module, so the shipped daemon's dependency graph stays small.

### The three test tiers

| Tier        | Location                         | Build tag                         | Run with                 |
|-------------|----------------------------------|-----------------------------------|--------------------------|
| Unit        | `pkg/**/*_test.go` (main module) | `test`                            | `make test`              |
| E2E         | `test/e2e/` (this module)        | `e2e`                             | `make test-e2e`          |
| Integration | `test/integration/`              | `ignore` (stripped by the runner) | `make integration-tests` |

## Running Tests

From the repository root:

```bash
# Run short tests
make test-e2e

# Run long tests only (currently matches zero specs -- see Test Labels below)
GINKGO_LABEL_FILTER="long" make test-e2e

# Run with custom image
E2E_IMAGE_TAG=dev E2E_IMAGE_NAME=myregistry/system-agent make test-e2e

# Keep cluster after tests for debugging
SKIP_RESOURCE_CLEANUP=true make test-e2e
```

## Configuration

### Environment Variables

| Variable                        | Description                                     | Default                                            |
|---------------------------------|-------------------------------------------------|----------------------------------------------------|
| `E2E_IMAGE_NAME`                | System-agent image name                         | `rancher/system-agent`                             |
| `E2E_IMAGE_TAG`                 | Image tag to test                               | `e2e-test`                                         |
| `E2E_KIND_CLUSTER_NAME`         | Kind cluster name                               | `system-agent-e2e`                                 |
| `USE_EXISTING_CLUSTER`          | Skip Kind cluster creation, use current context | `false`                                            |
| `SKIP_RESOURCE_CLEANUP`         | Preserve cluster after tests                    | `false`                                            |
| `E2E_ARTIFACTS`                 | Output directory for Ginkgo artifacts/JUnit XML | `_artifacts` (repo root: `$(ROOT_DIR)/_artifacts`) |
| `GINKGO_LABEL_FILTER`           | Ginkgo label filter                             | `short`                                            |
| `GINKGO_NODES`                  | Parallel test nodes                             | `1`                                                |
| `GINKGO_TIMEOUT`                | Overall test timeout                            | `30m`                                              |
| `GINKGO_POLL_PROGRESS_AFTER`    | Start printing progress reports after this long | `10m`                                              |
| `GINKGO_POLL_PROGRESS_INTERVAL` | Interval between progress reports               | `1m`                                               |

### Test Labels

Tests are categorized with labels for selective execution:

- `short`: Quick tests for CI
- `long`: Extended tests that take longer to run (nightly). **No spec carries this label today**, so `GINKGO_LABEL_FILTER="long"` currently matches zero specs and exits green — see Known Coverage Gaps below.

## Known Coverage Gaps

Not covered by this suite today — worth keeping in mind when triaging a production issue that "should have been caught by e2e":

- **Local plan mode** (`pkg/localplan`, `.plan` files on disk) has no e2e coverage. Every spec here only exercises remote mode (`pkg/k8splan`, plan delivered via a watched Secret). Local mode currently has unit-test coverage only.
- **TLS/mTLS and client-certificate probes** (`pkg/prober`) aren't exercised — `probes_test.go` only covers plain HTTP probes against the in-cluster test server.
- **Malformed/invalid plan JSON** isn't tested at this layer (only via unit tests in `pkg/applyinator`).
- **Agent restart / crash recovery of an *executing* plan** is only covered by the separate, heavier `test/integration` suite (`Test_SystemAgent_ForceApplyOnRestart`, `Test_SystemAgent_PlanState_CrashRecovery`), not by this Kind-based e2e suite. Restart while a plan is *held* is covered here: `pause_test.go`'s `should not execute anything across an agent restart while the plan is held` restarts the agent mid-hold and asserts that the plan is not resumed across the restart, that the checkpoint survives it, and that the eventual unpause re-runs nothing the checkpoint already accounted for.
- **The `long` label is unused.** Every `Describe` in this suite carries `framework.ShortTestLabel`, so a nightly job filtering on `long` runs nothing and reports success. Until long-running specs exist, nightly should run with an empty `GINKGO_LABEL_FILTER` (which runs all specs).
