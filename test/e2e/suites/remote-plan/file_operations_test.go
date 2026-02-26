//go:build e2e

package remoteplan_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - File Operations", Label(framework.ShortTestLabel), func() {
	It("should create multiple files in a single plan", func() {
		ctx := context.Background()

		By("Creating a plan with three files")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-multi-1.txt", "file one", "0644").
			WithFile("/tmp/e2e-multi-2.txt", "file two", "0644").
			WithFile("/tmp/e2e-multi-3.txt", "file three", "0644").
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		By("Waiting for applied-checksum")
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying all files were created with correct content")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		for _, tc := range []struct{ path, content string }{
			{"/tmp/e2e-multi-1.txt", "file one"},
			{"/tmp/e2e-multi-2.txt", "file two"},
			{"/tmp/e2e-multi-3.txt", "file three"},
		} {
			stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
				framework.E2ENamespace, podName, framework.AgentContainerName,
				[]string{"cat", tc.path})
			Expect(err).NotTo(HaveOccurred(), "Failed to read %s", tc.path)
			Expect(stdout).To(Equal(tc.content),
				"File %s content mismatch", tc.path)
		}
	})

	It("should create a directory via WithDirectory", func() {
		ctx := context.Background()

		By("Creating a plan with a directory")
		plan := framework.NewPlan().
			WithDirectory("/tmp/e2e-test-dir").
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		By("Waiting for applied-checksum")
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying the directory exists inside the pod")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"stat", "-c", "%F", "/tmp/e2e-test-dir"})
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(stdout)).To(Equal("directory"))
	})

	It("should create a file with custom permissions (0755)", func() {
		ctx := context.Background()

		By("Creating a plan with a file with 0755 permissions")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-perm-file.txt", "perm test", "0755").
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		By("Waiting for applied-checksum")
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying the file permissions via stat")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"stat", "-c", "%a", "/tmp/e2e-perm-file.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(stdout)).To(Equal("755"))
	})

	It("should delete a file via WithDeleteFile", func() {
		ctx := context.Background()

		By("Phase 1: Creating a file via plan")
		plan1 := framework.NewPlan().
			WithFile("/tmp/e2e-delete-target.txt", "to be deleted", "0644").
			Build()

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan1)
		Expect(err).NotTo(HaveOccurred())

		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)

		By("Verifying the file exists")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-delete-target.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("to be deleted"))

		By("Phase 2: Deleting the file via plan with delete action")
		plan2 := framework.NewPlan().
			WithDeleteFile("/tmp/e2e-delete-target.txt").
			Build()

		err = framework.UpdatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan2)
		Expect(err).NotTo(HaveOccurred())

		expectedChecksum2 := fmt.Sprintf("%x", sha256.Sum256(plan2))

		appliedChecksum := framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum",
			func(val []byte) bool { return string(val) == expectedChecksum2 },
			120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum2))

		By("Verifying the file is gone")
		_, _, err = framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"stat", "/tmp/e2e-delete-target.txt"})
		Expect(err).To(HaveOccurred(), "File should have been deleted")
	})

	It("should idempotently write the same file", func() {
		ctx := context.Background()

		By("Creating a plan with a file")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-idempotent.txt", "idempotent content", "0644").
			Build()

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		By("Applying the plan for the first time")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying the file content")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-idempotent.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("idempotent content"))

		By("Waiting through additional re-enqueue cycles")
		time.Sleep(10 * time.Second)

		By("Verifying the checksum is unchanged and file content is still correct")
		checksum2 := framework.GetAppliedChecksum(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(checksum2).To(Equal(expectedChecksum))

		stdout, _, err = framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-idempotent.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("idempotent content"))
	})

	It("should auto-create nested parent directories for deep file paths", func() {
		ctx := context.Background()

		By("Creating a plan with a file at a deep path")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-nested/a/b/c/deep-file.txt", "deep content", "0644").
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		By("Waiting for applied-checksum")
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying parent directories were created")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"stat", "-c", "%F", "/tmp/e2e-nested/a/b/c"})
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(stdout)).To(Equal("directory"))

		By("Verifying the file content")
		stdout, _, err = framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-nested/a/b/c/deep-file.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("deep content"))
	})
})
