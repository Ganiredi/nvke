// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// kapsBenchTimeout is also the token's lifetime, because nothing refreshes a token in a file.
	kapsBenchTimeout = 30 * time.Minute
	// prometheusInstallTimeout is how long helm waits for kube-prometheus-stack to become ready.
	prometheusInstallTimeout = 10 * time.Minute
	// toolOutputLines is how many lines of a failed tool's output its error carries.
	toolOutputLines = 40
	// toolWaitDelay bounds the wait for a killed tool's output pipe to close.
	toolWaitDelay = 10 * time.Second
	// kapsBenchResults is written next to qualify-e2e.log, so qualify local collects it with the run.
	kapsBenchResults = "kaps-bench-results.tar.gz"
)

// kubePrometheusStackValues is embedded so the compiled suite needs no checkout beside it.
//
//go:embed testdata/kube-prometheus-stack-values.yaml
var kubePrometheusStackValues []byte

// validateTenantKapsBench runs each kaps-bench run spec on the tenant after its checks pass.
func validateTenantKapsBench(test *tenantTest, failed func() bool) {
	It("runs kaps-bench on the tenant", func(ctx context.Context) {
		if failed() {
			Skip("an earlier check on this tenant failed")
		}
		Expect(requireKapsBenchTools(kapsBenchRunSpecs, kapsBenchDir, exec.LookPath)).To(Succeed())

		work := GinkgoT().TempDir()
		kubeconfig := filepath.Join(work, "kubeconfig")
		// env is the tools' whole environment, since the runner's own carries the lane's secrets.
		env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "TMPDIR=" + work, "KUBECONFIG=" + kubeconfig}

		By("Writing a relayed admin kubeconfig for the tenant")
		relayed := expectSucceed(test.tenantClients.RelayedAdminKubeconfig(ctx, test.cluster.Name, kapsBenchTimeout))
		Expect(os.WriteFile(kubeconfig, relayed, 0o600)).To(Succeed())

		By("Installing kube-prometheus-stack into namespace monitoring with helm")
		values := filepath.Join(work, "kube-prometheus-stack-values.yaml")
		Expect(os.WriteFile(values, kubePrometheusStackValues, 0o600)).To(Succeed())
		Expect(runTool(ctx, work, env, "helm", prometheusInstallArgs(values)...)).To(Succeed())

		By("Writing the cluster context kaps-bench records each run under")
		admin := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		var kubeSystem corev1.Namespace
		Expect(admin.Get(ctx, client.ObjectKey{Name: kubeSystemNamespace}, &kubeSystem)).To(Succeed())
		clusterContext := filepath.Join(work, "cluster-context.yaml")
		document := expectSucceed(kapsBenchClusterContext(test.cluster.Name, string(kubeSystem.UID)))
		Expect(os.WriteFile(clusterContext, document, 0o600)).To(Succeed())

		outputDir := filepath.Join(work, "output")
		// Cleanups run in reverse order, so the results are archived before the temp dir is removed.
		DeferCleanup(func() {
			if _, err := os.Stat(outputDir); err != nil {
				return
			}
			if err := archiveKapsBenchResults(outputDir, kapsBenchResults); err != nil {
				GinkgoLogr.Error(err, "could not archive kaps-bench's results")
				return
			}
			GinkgoLogr.Info("archived kaps-bench's results", "path", kapsBenchResults)
		})
		for _, spec := range kapsBenchRunSpecs {
			By("Running kaps-bench run spec " + spec)
			args := kapsBenchRunArgs(spec, kubeconfig, clusterContext, outputDir)
			Expect(runTool(ctx, kapsBenchDir, env, "kaps-bench", args...)).To(Succeed(), "kaps-bench run spec %s should pass", spec)
		}
	}, Label("kaps-bench"), NodeTimeout(kapsBenchTimeout))
}

// parseKapsBenchRunSpecs returns the comma separated run specs, refusing an empty one.
func parseKapsBenchRunSpecs(value string) ([]string, error) {
	specs := strings.Split(value, ",")
	if slices.Contains(specs, "") {
		return nil, fmt.Errorf("-e2e.kaps-bench-run-specs %q has an empty run spec", value)
	}
	return specs, nil
}

// requireKapsBenchTools returns an error naming each tool or folder the run specs need but lack.
func requireKapsBenchTools(specs []string, dir string, lookPath func(file string) (string, error)) error {
	if len(specs) == 0 {
		return nil
	}
	var errs []error
	if dir == "" {
		errs = append(errs, errors.New("-e2e.kaps-bench-dir names no kaps-bench folder, so set E2E_KAPS_BENCH_DIR"))
	} else if info, err := os.Stat(dir); err != nil {
		errs = append(errs, fmt.Errorf("-e2e.kaps-bench-dir: %w", err))
	} else if !info.IsDir() {
		errs = append(errs, fmt.Errorf("-e2e.kaps-bench-dir %s is not a folder", dir))
	}
	for _, tool := range []string{"kaps-bench", "helm"} {
		if _, err := lookPath(tool); err != nil {
			errs = append(errs, fmt.Errorf("%s is not on PATH: %w", tool, err))
		}
	}
	return errors.Join(errs...)
}

// prometheusInstallArgs returns helm's arguments for Prometheus in the namespace the run specs name.
func prometheusInstallArgs(values string) []string {
	return []string{
		"upgrade", "--install", "prometheus", "kube-prometheus-stack",
		"--repo", "https://prometheus-community.github.io/helm-charts", "--version", "76.5.0",
		"--namespace", "monitoring", "--create-namespace",
		"--values", values,
		"--wait", "--timeout", prometheusInstallTimeout.String(),
	}
}

// kapsBenchRunArgs returns the arguments that run one run spec and keep its results in outputDir.
func kapsBenchRunArgs(spec, kubeconfig, clusterContext, outputDir string) []string {
	return []string{
		"run", spec,
		"--kubeconfig", kubeconfig,
		"--cluster-context", clusterContext,
		"--output-dir", outputDir,
		"--local-publisher",
	}
}

// kapsBenchClusterContext returns the cluster context kaps-bench records a tenant's runs under.
func kapsBenchClusterContext(name, uid string) ([]byte, error) {
	return yaml.Marshal(map[string]map[string]string{"cluster": {"name": name, "uuid": uid}})
}

// runTool returns an error carrying the tail of the tool's output when the tool fails.
func runTool(ctx context.Context, dir string, env []string, name string, args ...string) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = io.MultiWriter(GinkgoWriter, &out)
	cmd.Stderr = cmd.Stdout
	// Killing the whole group stops what the tool started, before the tenant is deleted under it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = toolWaitDelay
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	return commandError(cmd, err, []byte(lastOutputLines(out.String(), toolOutputLines)))
}

// lastOutputLines returns the last n lines of a tool's output.
func lastOutputLines(output string, n int) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// archiveKapsBenchResults writes every file under dir into the tar.gz archive at dest.
func archiveKapsBenchResults(dir, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := errors.Join(tw.AddFS(os.DirFS(dir)), tw.Close(), gz.Close(), f.Close()); err != nil {
		return fmt.Errorf("archiving %s into %s: %w", dir, dest, err)
	}
	return nil
}
