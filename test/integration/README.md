# System Agent Integration Tests

## Overview

These tests exercise system-agent in Rancher's real provisioning flow by reusing the existing `tests/v2prov` framework.
They run alongside Rancher's Machine Provisioning and pre-bootstrap coverage under the same Rancher instance, which avoids provisioning a second cluster just for system-agent coverage.

This differs from the kind based [e2e suite](../README.md):

- `test/e2e` validates system-agent behavior inside this repository's own test harness.
- `test/integration` validates system-agent behavior in the Rancher provisioning flow, with real v2prov helpers and a real Rancher server.

The CI entrypoint is:

```bash
make integration-tests
```

That target runs `scripts/integration-tests`, which does the following:

1. Builds the `rancher-system-agent` binary from the current branch.
2. Fetches Rancher's provisioning test tree and a Rancher binary via `scripts/fetch-provisioning-tests`.
3. Copies this repository's integration test files into Rancher's `tests/v2prov/tests/systemagent` package.
4. Strips the leading `//go:build ignore` line from copied tests.
5. Patches Rancher's provisioning test script to use local system-agent artifacts instead of downloading release binaries.
6. Runs Rancher's provisioning tests against a real Rancher instance.

## Why These Tests Use `//go:build ignore`

Integration tests in this directory import Rancher's `tests/v2prov` packages. Keeping those imports in this repository's normal Go module would pull in Rancher's large dependency tree and break ordinary local `go test` and build flows.

To avoid that, integration tests here are intentionally kept out of the normal package walk with:

```go
//go:build ignore
```

The integration runner removes that line only after copying the tests into Rancher's module tree, where the `tests/v2prov` imports are valid.

If a new integration test keeps the build tag after the copy step, Rancher's runner will silently skip it. That is why `scripts/integration-tests` strips the first `//go:build ignore` line from copied files.


## Naming Convention

The default test regex in `scripts/integration-tests` is:

```bash
^Test_(Provisioning_MP|PreBootstrap|SystemAgent)_.*$
```

That means:

- `Test_Provisioning_MP_*` and `Test_PreBootstrap_*` are Rancher-owned tests that already live in the Rancher repository.
- `Test_SystemAgent_*` are system-agent-owned tests that live in this repository and are copied into Rancher's test tree at runtime.

New tests added here should use the `Test_SystemAgent_*` prefix so they are included by default.

## How To Add A New Integration Test

1. Add a new `*_test.go` file under `test/integration`.
2. Start the file with `//go:build ignore`.
3. Write the test against Rancher's `tests/v2prov` helpers, matching the existing tests in this directory.
4. Name the test function `Test_SystemAgent_*` so it matches the default regex.
5. Run `make integration-tests` to validate the new test in the Rancher-backed flow.