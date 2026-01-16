---
title: System Agent Improved Plans
authors:
  - "@alexander-demicev"
reviewers:
  - TBD
creation-date: 2026-01-15
last-updated: 2026-01-16
status: provisional
replaces:
superseded-by:
---

# System Agent Improved Plans

## Table of Contents

<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->

- [System Agent Improved Plans](#system-agent-improved-plans)
  - [Table of Contents](#table-of-contents)
  - [Glossary](#glossary)
  - [Summary](#summary)
  - [Motivation](#motivation)
    - [Current Limitations](#current-limitations)
    - [Goals](#goals)
    - [Non-Goals/Future Work](#non-goalsfuture-work)
  - [Proposal](#proposal)
    - [User Stories](#user-stories)
    - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
      - [NodePlan CRD](#nodeplan-crd)
      - [File Locking and Multi-NodePlan Coordination](#file-locking-and-multi-nodeplan-coordination)
      - [Preventing Spec Changes During Execution](#preventing-spec-changes-during-execution)
      - [Content References for Large Files](#content-references-for-large-files)
      - [OCI-Packaged Plans with Signature Verification](#oci-packaged-plans-with-signature-verification)
      - [Multi-Party Coordination](#multi-party-coordination)
    - [Security Model](#security-model)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Alternatives](#alternatives)
  - [Upgrade Strategy](#upgrade-strategy)
  - [Additional Details](#additional-details)
    - [Test Plan](#test-plan)
    - [Graduation Criteria](#graduation-criteria)
  - [Implementation History](#implementation-history)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->

## Glossary

- **System Agent**: Node-level agent that executes plans on managed cluster nodes
- **Plan**: A set of files, instructions, and health checks to be applied on a node
- **Applyinator**: Current plan execution engine within system-agent
- **NodePlan CRD**: Proposed Custom Resource Definition for delivering plans via Kubernetes API
- **Day 2 Operations**: Post-installation operations like etcd restore, certificate rotation, upgrades

## Summary

This proposal introduces improvements to the system-agent codebase. The system-agent is a component that runs on cluster nodes managed by Rancher and executes plans for bootstrapping and Day 2 operations (etcd restore, cert rotation). While currently deployed on RKE2/K3s clusters, the goal is to extend system-agent to run on all cluster types within Rancher, including imported clusters and those managed by CAPI.

The current implementation has several limitations:
- Plan delivery via unstructured Secrets.
- Limited extensibility for custom health checks, retry strategies, and instruction dependencies
- No verification of plan content.

This proposal introduces:
1. **NodePlan CRD** as a new, structured, Kubernetes-native plan delivery mechanism
2. **New extensibility features**: custom probes, retry strategies, preflight checks, instruction dependencies
3. **Multi-NodePlan support** with file-based mutual exclusion for multi-party coordination
4. **OCI-packaged plans** with signature verification for secure execution

The key principle is to **keep the agent as simple as possible**, with a clear separation of concerns between the agent (local execution) and the top-level orchestrator (Rancher). The agent executes plans; orchestration complexity stays in Rancher's management plane.

## Motivation

### Current Limitations

1. **Extensibility**: Adding features like custom probes or retry strategies requires big changes throughout the codebase
2. **Observability**: Limited structured feedback for operations
3. **Multi-party coordination**: Only Rancher can currently manage plans; other parties have no safe coordination mechanism
4. **Secret size limits**: Plan content can exceed 1MiB Secret limit due to inline base64-encoded files
5. **Schema validation**: No validation of plan structure until runtime execution
6. **Security**: No verification of plan content before execution

### Goals

- Introduce NodePlan CRD as a new alternative plan delivery mechanism with proper schema validation
- Enable new features: custom probes (HTTP/HTTPS, FileExists), configurable retry, preflight checks, instruction dependencies
- Support multiple NodePlans per node with file-lock-based mutual exclusion
- Keep the current Secret-based mechanism working exactly as it was before
- Improve observability for Day 2 operations through structured status reporting

### Non-Goals/Future Work

- Multi-node coordination, init node election, rollback orchestration, dependency graphs, topology awareness, drain/cordon, upgrade sequencing
- Deprecate current Secret-based mechanisms
- Reduce RBAC resource usage
- System-agent lifecycle management (installation, self-update)

## Proposal

### User Stories

1. **Structured Plan Delivery**: As a developer, I want plans delivered via a structured CRD (NodePlan) with proper schema validation and status reporting, so I can observe plan execution without parsing compressed blobs.

2. **Secure Execution**: As a user, I want plan content verified via OCI signatures before execution, so I can ensure only trusted plans run on my nodes.

3. **Multiple Plan Creators**: As a developer I want multiple parties (e.g., Rancher embedded Day 2 operations + external updaters like CAPRKE2) to safely create and manage plans for the same node without conflicts.

4. **Custom Health Checks**: As a developer, I want to define custom probes (HTTPS with certificates, file existence checks) so plans can wait for services to be healthy before proceeding.

### Implementation Details/Notes/Constraints

This proposal introduces a new CRD-based plan delivery mechanism while preserving the existing Secret-based approach. The two paths operate independently:

**Current Path**: Secret/File watchers → existing Applyinator. Remains untouched.
- Used by existing Rancher deployments
- No changes to behavior or implementation
- Continues to work exactly as before

**New CRD Path**: NodePlan CRD → controller-runtime reconciler → new execution engine.
- New alternative for plan delivery
- Provides structured schema, status reporting, and new features
- Execution engine built with a similar approach to Applyinator, but extended to support new capabilities

All new features (custom probes, retry strategies, preflight checks, instruction dependencies, OCI signature verification) are available only in the CRD path. The current Secret/File path remains unchanged for backward compatibility, ensuring zero impact on existing deployments.

#### NodePlan CRD

The following is a **draft schema** for the NodePlan CRD:

```yaml
apiVersion: agent.cattle.io/v1alpha1
kind: NodePlan
metadata:
  name: node-abc123-rancher-bootstrap
  labels:
    cattle.io/node: node-abc123  # Required for filtering
spec:
  plan:
    files: [...]
    instructions: [...]
    probes:
      apiserver-health:
        httpGet:
          url: "https://localhost:6443/healthz"
          tlsConfig:
            caSecretRef:
              name: apiserver-ca
              key: ca.crt
      config-exists:
        fileExists:
          path: "/etc/rancher/rke2/config.yaml"
  
  retryStrategy:
    maxAttempts: 10
    backoffMultiplier: 2.0
  
  preflightChecks:
    - name: apiserver-ready
      probe: { httpGet: {...} }
      required: true
  
  locking:
    enabled: true  # Default
  
  execution:
    timeout: "30m"

status:
  observedGeneration: 5
  phase: Pending|Executing|Applied|Failed|Cancelled
  conditions: [...]
  probeStatuses: {...}
  executionState:
    started: "2026-01-15T10:00:00Z"
    lockHolder: "node-abc123-rancher-bootstrap"  # Which NodePlan holds the lock
```

**Note on TLS configuration**: Certificate data can be referenced from Secrets or ConfigMaps via `caSecretRef` or `caConfigMapRef`, avoiding inline base64 blobs. This
approach might complicate the RBAC setup.

#### File Locking and Multi-NodePlan Coordination

Multiple NodePlans can exist for the same node (created by Rancher, or other parties). To ensure only one plan executes at a time, the agent uses file-based locking.

**How it works**:
- Agent watches ALL NodePlans with label `cattle.io/node: <nodeName>`
- Before executing, reconciler attempts to acquire a file lock at `/var/lib/rancher/agent/plan.lock`
- If lock is held by another plan, reconciler returns and requeues (standard controller-runtime pattern)
- First to acquire lock wins; execution order is **not guaranteed**
- Lock file contains metadata (NodePlan name, PID, timestamp) for debugging and stale lock detection

**Lock state recovery**: Following the Kubernetes controller pattern, the reconciler must be able to recover state on restart. The lock file metadata and NodePlan status, allow the agent to determine if it was the previous lock holder and either resume or release the stale lock.

#### Preventing Spec Changes During Execution

**The problem**: During plan execution (which can take minutes), if the spec changes between a failed execution and a retry, the retry would execute a different plan than what originally failed. This breaks the assumption that retry = re-execute the same plan.

**Why this matters**: Plans have a list of instructions. If the spec changes mid-execution, a retry could execute a completely different list, leading to unpredictable behavior or partial state application.

The webhook prevents spec changes while `status.phase == Executing`. This ensures:
- Retries always execute the same plan that failed
- Instructions list remains consistent across retries
- Emergency override available via `agent.cattle.io/force-update: true` annotation

**Deployment model**: System-agent provides the webhook validation logic as a library package. The top-level orchestrator (Rancher, CAPRKE2) imports and deploys this webhook as part of its existing webhook infrastructure. The webhook service runs in the orchestrator's control plane, not on the nodes.

#### Content References for Large Files

To address the 1MiB Secret/CRD size limit when plans contain large files (e.g., container images), the CRD supports content references:

```yaml
spec:
  plan:
    files:
      - path: "/var/lib/rancher/images/core.tar"
        contentRef:
          oci: "registry.example.com/files/core:v1"
          digest: "sha256:abc123..."
          verification:
            mode: enforce  # disabled|warn|enforce
```

Instead of inline base64 content, the agent fetches the file from the referenced OCI registry. The digest ensures integrity, and optional signature verification provides authenticity.

#### OCI-Packaged Plans with Signature Verification

As an alternative to inline plan definitions, entire plans can be packaged as OCI artifacts and signed using cosign. This provides:
- **Integrity**: Plans cannot be tampered with in transit
- **Authenticity**: Plans are verified to come from a trusted source
- **Auditability**: Signature verification can be logged

**OCI Plan Format**:
```
registry.example.com/rancher/plans/rke2-bootstrap:v1.28
├── run.sh              # Entry point script (always executed)
└── files/              # Optional: files to be written
```

The agent pulls the OCI artifact, verifies the signature (keyless or key-based), extracts and executes `run.sh`.

**Example NodePlan using OCI-packaged plan**:
```yaml
apiVersion: agent.cattle.io/v1alpha1
kind: NodePlan
metadata:
  name: node-abc123-oci-plan
  labels:
    cattle.io/node: node-abc123
spec:
  planRef:
    oci: "registry.example.com/rancher/plans/rke2-bootstrap:v1.28"
    verification:
      mode: enforce  # disabled|warn|enforce
  
  retryStrategy:
    maxAttempts: 10
    backoffMultiplier: 2.0
  
  locking:
    enabled: true
  
  execution:
    timeout: "30m"
```

When using `spec.planRef`, the `spec.plan` field is not used (mutually exclusive).

#### Multi-Party Coordination

Multiple parties (Rancher and others) can create NodePlans for the same node:

```
NodePlans for node-abc123:
├── node-abc123-rancher-bootstrap    (phase: Applied)
├── node-abc123-external-upgrade     (phase: Pending) ─┐
└── node-abc123-rancher-day2-op      (phase: Pending) ─┴─► Race for lock
```

**Key design decisions**:
- Multiple NodePlans CAN exist per node (different producers)
- Only ONE executes at a time (enforced by file lock)
- Execution order is **NOT guaranteed** — first to get lock wins (optimistic concurrency)
- Each producer manages their own NodePlan(s) independently
- Standard controller-runtime reconcile pattern (no custom queue)

**Preventing deadlocks**: A potential problem with file locking is deadlock if a lock is never released (e.g., agent crashes while holding the lock). The system prevents this through:
- **Stale lock detection**: Lock file contains metadata (NodePlan name, PID, timestamp)
- **Automatic cleanup**: If lock holder PID no longer exists, lock is considered stale and can be acquired
- **Timeout mechanism**: Locks older than a configurable threshold are automatically released
- **Recovery on restart**: Agent checks lock status on startup and can recover from its own crashes

### Security Model

The current system-agent assumes plans are safe and executes them without verification. This proposal introduces security improvements while maintaining simplicity.

**Current state**: Plans are delivered via Secrets and executed without validation. The agent trusts that whatever is in the plan Secret is legitimate. This works in controlled environments where only Rancher has access to create/update plan Secrets, but provides no defense against compromised credentials or supply chain attacks.

**Proposed improvements**:

- **OCI signature verification**: Plans packaged as OCI artifacts can be signed with cosign and verified before execution, ensuring authenticity and integrity.

### Risks and Mitigations

TBD

## Alternatives

TBD

## Upgrade Strategy

TBD

## Additional Details

### Test Plan

TBD

### Graduation Criteria

TBD

## Implementation History

TBD