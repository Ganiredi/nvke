// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseKapsBenchRunSpecs(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected []string
	}{
		{name: "one run spec", value: "smoke/smoke.yaml", expected: []string{"smoke/smoke.yaml"}},
		{name: "two run specs in order", value: "smoke/smoke.yaml,netperf/hostnet-stream.yaml", expected: []string{"smoke/smoke.yaml", "netperf/hostnet-stream.yaml"}},
		{name: "an empty value", value: ""},
		{name: "an empty entry", value: "smoke/smoke.yaml,"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs, err := parseKapsBenchRunSpecs(tc.value)
			if tc.expected == nil {
				t.Log("Testing: an empty entry is refused, so kaps-bench is never run with an empty run spec")
				require.ErrorContains(t, err, "empty")
				return
			}
			t.Log("Testing: the run specs come back in the order given")
			require.NoError(t, err)
			require.Equal(t, tc.expected, specs)
		})
	}
}

func TestRequireKapsBenchTools(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kaps-bench.yaml")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	specs := []string{"smoke/smoke.yaml"}
	found := func(file string) (string, error) { return "/usr/local/bin/" + file, nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	tests := []struct {
		name     string
		specs    []string
		dir      string
		lookPath func(string) (string, error)
		expected []string
	}{
		{name: "no run specs needs nothing", dir: "", lookPath: missing},
		{name: "everything is here", specs: specs, dir: dir, lookPath: found},
		{name: "no folder named", specs: specs, dir: "", lookPath: found, expected: []string{"E2E_KAPS_BENCH_DIR"}},
		{name: "a folder that does not exist", specs: specs, dir: filepath.Join(dir, "absent"), lookPath: found, expected: []string{"-e2e.kaps-bench-dir"}},
		{name: "a file in place of the folder", specs: specs, dir: file, lookPath: found, expected: []string{"is not a folder"}},
		{name: "neither tool on PATH", specs: specs, dir: dir, lookPath: missing, expected: []string{"kaps-bench is not on PATH", "helm is not on PATH"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Testing: each missing piece is named in the one error")
			err := requireKapsBenchTools(tc.specs, tc.dir, tc.lookPath)
			if len(tc.expected) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tc.expected {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

func TestPrometheusInstallArgs(t *testing.T) {
	t.Log("Testing: helm installs the pinned kube-prometheus-stack into monitoring and waits for it")
	require.Equal(t, []string{
		"upgrade", "--install", "prometheus", "kube-prometheus-stack",
		"--repo", "https://prometheus-community.github.io/helm-charts", "--version", "76.5.0",
		"--namespace", "monitoring", "--create-namespace",
		"--values", "/w/kube-prometheus-stack-values.yaml",
		"--wait", "--timeout", "10m0s",
	}, prometheusInstallArgs("/w/kube-prometheus-stack-values.yaml"))
}

func TestKapsBenchRunArgs(t *testing.T) {
	t.Log("Testing: kaps-bench runs the spec against the tenant and publishes locally")
	require.Equal(t, []string{
		"run", "smoke/smoke.yaml",
		"--kubeconfig", "/w/kubeconfig",
		"--cluster-context", "/w/cluster-context.yaml",
		"--output-dir", "/w/output",
		"--local-publisher",
	}, kapsBenchRunArgs("smoke/smoke.yaml", "/w/kubeconfig", "/w/cluster-context.yaml", "/w/output"))
}

func TestKapsBenchClusterContext(t *testing.T) {
	t.Log("Testing: the cluster context names the tenant and the UID of its kube-system namespace")
	document, err := kapsBenchClusterContext("az41-ganesh-test", "209b2b00-e17b-4d05-8073-dccbed7ef5f0")
	require.NoError(t, err)
	require.Equal(t, "cluster:\n  name: az41-ganesh-test\n  uuid: 209b2b00-e17b-4d05-8073-dccbed7ef5f0\n", string(document))
}

func TestRunTool(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir}

	t.Log("Testing: a tool that exits 0 passes")
	require.NoError(t, runTool(t.Context(), dir, env, "sh", "-c", "echo installed"))

	t.Log("Testing: a tool that exits non-zero fails with its command and the end of its output")
	err := runTool(t.Context(), dir, env, "sh", "-c", "echo preflight passed; echo run failed; exit 3")
	_, isExit := errors.AsType[*exec.ExitError](err)
	require.True(t, isExit, "the exit status should be in the error chain")
	require.ErrorContains(t, err, "sh -c echo preflight passed")
	require.ErrorContains(t, err, "preflight passed\nrun failed")

	t.Log("Testing: the tool sees only the environment it was given")
	require.NoError(t, runTool(t.Context(), dir, env, "sh", "-c", `[ "$HOME" = "`+dir+`" ] && [ -z "${NGC_API_KEY:-}" ]`))

	t.Log("Testing: a tool stopped by its context says why the context ended")
	timedOut := errors.New("the kaps-bench step ran out of time")
	ctx, cancel := context.WithTimeoutCause(t.Context(), 200*time.Millisecond, timedOut)
	defer cancel()
	err = runTool(ctx, dir, env, "sh", "-c", "sleep 30")
	require.ErrorIs(t, err, timedOut)
	require.ErrorContains(t, err, "running sh -c sleep 30: the kaps-bench step ran out of time: ")
}

func TestArchiveKapsBenchResults(t *testing.T) {
	t.Log("Doing: an output dir laid out the way kaps-bench writes one")
	output := t.TempDir()
	run := filepath.Join(output, "01a1c2e3-1f76-726d-8550-5149212bdcb2")
	require.NoError(t, os.MkdirAll(run, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "preflight.json"), []byte(`{"passed":true}`), 0o600))
	archive := filepath.Join(t.TempDir(), "kaps-bench-results.tar.gz")

	t.Log("Testing: the archive holds every file under the output dir by its relative path")
	require.NoError(t, archiveKapsBenchResults(output, archive))
	f, err := os.Open(archive)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[hdr.Name] = string(content)
		}
	}
	require.Equal(t, map[string]string{"01a1c2e3-1f76-726d-8550-5149212bdcb2/preflight.json": `{"passed":true}`}, files)
}
