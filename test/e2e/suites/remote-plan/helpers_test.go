//go:build e2e

package remoteplan_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/test/framework"
)

// instructionWedgeCapSeconds bounds how long an instruction in these specs can block.
//
// Applyinator.Apply holds the applyinator mutex and runs synchronously in the controller's single
// OnChange worker, so an instruction that outlives its spec does more than leak a shell: it wedges
// the agent. Other specs wait on framework.WaitTimeout (120s), so an instruction that can block
// longer than that can turn one failure into a cascade. This cap is the third and final line of
// defense - after the interrupt under test and releaseGateOnCleanup - and the only one that still
// applies if both of those fail.
const instructionWedgeCapSeconds = 60

// blockingScript returns a shell snippet that waits for gate to appear, then gives up after
// instructionWedgeCapSeconds.
//
// The gate makes the pause specs deterministic: the spec can verify that the agent observed the
// annotation before allowing the running instruction to return, eliminating the timing window.
// The cap bounds the damage if that synchronization or the cleanup release fails. Both are
// load-bearing.
func blockingScript(gate string) string {
	return fmt.Sprintf("i=0; while [ ! -e %s ] && [ $i -lt %d ]; do sleep 1; i=$((i+1)); done",
		gate, instructionWedgeCapSeconds)
}

// releaseGateOnCleanup opens the gate regardless of how the spec ends, so an instruction blocked on
// it is released as soon as the spec finishes rather than waiting for the cap.
//
// This uses exec rather than the Secret on purpose. Ginkgo runs AfterEach nodes before
// DeferCleanup nodes: internal/group.go:246-331 first runs with includeDeferCleanups=false, covering
// the AfterEach/JustAfterEach/AfterAll nodes, then repeats with it set to true. By the time this
// runs, the suite's AfterEach has already deleted the plan Secret. Any cleanup that tried to
// release the instruction through that Secret - for example, by setting the cancel annotation - would
// therefore be a silent no-op. This is also why blockingScript has a cap rather than relying solely
// on cleanup.
//
// The body deliberately makes no assertions. A Gomega failure here could abort cleanup before it
// opens the gate, which is exactly what this helper exists to prevent. For the same reason, the pod
// name is obtained through agentPodName rather than framework.KubectlGetPodName.
func releaseGateOnCleanup(gate string) {
	DeferCleanup(func() {
		ctx := context.Background()
		podName, ok := agentPodName(ctx)
		if !ok {
			return
		}
		_, _, _ = framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"/bin/sh", "-c", "touch " + gate})
	})
}

// agentPodName returns the agent pod's name without asserting on failure. This is intended for
// cleanup paths, where framework.KubectlGetPodName's internal Expect would be more disruptive than
// simply returning no name.
func agentPodName(ctx context.Context) (string, bool) {
	result := &framework.RunCommandResult{}
	framework.RunCommand(ctx, framework.RunCommandInput{
		Command: "kubectl",
		Args: []string{
			"--kubeconfig", kubeconfigPath, "get", "pods",
			"-l", framework.AgentLabel, "-n", framework.E2ENamespace,
			"-o", "jsonpath={.items[0].metadata.name}",
		},
	}, result)
	name := strings.TrimSpace(string(result.Stdout))
	return name, result.Error == nil && name != ""
}

// nodeFileExists reports whether path exists inside the agent container.
func nodeFileExists(ctx context.Context, podName, path string) bool {
	stdout := execInAgent(ctx, podName, fmt.Sprintf("if [ -e %s ]; then echo yes; else echo no; fi", path))
	return strings.TrimSpace(stdout) == "yes"
}

// nodeFileLineCount returns the number of lines in path inside the agent container, or 0 if the
// file does not exist. It counts the lines here rather than using wc so the specs only depend on
// /bin/sh and cat for node-side commands, both of which existing specs already use.
func nodeFileLineCount(ctx context.Context, podName, path string) int {
	content := nodeFileContent(ctx, podName, path)
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

// nodeFileContent returns path's contents inside the agent container, or "" when it does not
// exist. Surrounding whitespace is trimmed so callers can compare against a plain literal.
func nodeFileContent(ctx context.Context, podName, path string) string {
	return strings.TrimSpace(execInAgent(ctx, podName, fmt.Sprintf("cat %s 2>/dev/null || true", path)))
}

// execInAgent runs a shell snippet inside the agent container and returns its stdout.
//
// A kubectl failure fails the spec instead of being folded into the return value. This is
// deliberate: most callers use the result to assert that an instruction never ran, and treating
// "kubectl could not execute" as "the file is absent" would let a broken check pass vacuously.
// Gomega propagates a failed Expect out of an Eventually or Consistently poller rather than
// retrying it (internal/async_assertion.go:329-335 re-panics unless the poller takes a Gomega
// argument), so an exec failure surfaces immediately.
//
// Cleanup paths must not use this; see releaseGateOnCleanup.
func execInAgent(ctx context.Context, podName, script string) string {
	stdout, stderr, err := framework.KubectlExec(ctx, kubeconfigPath,
		framework.E2ENamespace, podName, framework.AgentContainerName,
		[]string{"/bin/sh", "-c", script})
	Expect(err).NotTo(HaveOccurred(), "kubectl exec failed running %q: %s", script, stderr)
	return stdout
}

// currentPlanState reads plan-state off the plan Secret.
func currentPlanState(ctx context.Context) planapi.PlanState {
	data := framework.GetSecretData(ctx, cl, framework.E2ENamespace, framework.PlanSecretName)
	return planapi.PlanState(data[planapi.PlanStateKey])
}
