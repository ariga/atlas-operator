// Copyright 2024 The Atlas Operator Authors.
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

package e2e_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
)

const (
	nsController = "atlas-operator-system"
	controller   = "deployment/atlas-operator-controller-manager"
)

func TestOperator(t *testing.T) {
	img := os.Getenv("IMG")
	if img == "" {
		img = "ariga/atlas-operator:e2e"
	}
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster == "" {
		kindCluster = "kind"
	}
	dir, err := getProjectDir()
	require.NoError(t, err)
	// Creating kubeconfig for the kind cluster
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	err = pipeFile(kubeconfig, func(f io.Writer) error {
		cmd := exec.Command("kind", "get", "kubeconfig", "--name", kindCluster)
		cmd.Stdout, cmd.Stderr = f, os.Stderr
		return cmd.Run()
	})
	require.NoError(t, err)
	// Kind run the command with kubeconfig set to kind cluster
	kind := func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		command := strings.Join(cmd.Args, " ")
		fmt.Fprintf(os.Stdout, "running: %s\n", command)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
		}
		return string(output), nil
	}
	// Deploying the controller-manager
	_, err = kind("make", "deploy", "install", fmt.Sprintf("IMG=%s", img))
	require.NoError(t, err)
	// Restarting the controller-manager, to ensure the latest image is used
	_, err = kind("kubectl", "rollout", "restart",
		"-n", nsController, controller)
	require.NoError(t, err)
	// Waiting for the controller-manager to be ready
	_, err = kind("kubectl", "rollout", "status",
		"-n", nsController, controller, "--timeout", "2m")
	require.NoError(t, err)
	// Running the test script
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("testdata", "atlas-schema"),
		Setup: func(e *testscript.Env) (err error) {
			e.Setenv("CONTROLLER_NS", nsController)
			e.Setenv("CONTROLLER", controller)
			// Sharing the atlas token with the test
			e.Setenv("ATLAS_TOKEN", os.Getenv("ATLAS_TOKEN"))
			// Ensure the test in running in the right kube context
			e.Setenv("KUBECONFIG", kubeconfig)
			// Creating a namespace for the test
			ns := fmt.Sprintf("e2e-%s-%d", strings.ToLower(t.Name()), time.Now().UnixMicro())
			e.Setenv("NAMESPACE", ns)
			_, err = kind("kubectl", "create", "namespace", ns)
			if err != nil {
				return err
			}
			e.Defer(func() {
				// Deleting the namespace after the test
				kind("kubectl", "delete", "namespace", ns)
			})
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			// expand reads a file and expands it with the environment variables
			"expand": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg {
					ts.Fatalf("unsupported: ! stdin")
				}
				if len(args) < 1 {
					ts.Fatalf("usage: expand filename")
				}
				w := ts.Stdout()
				// If the last argument is >, write to a file
				if l := len(args); l > 2 && args[l-2] == ">" {
					outPath := filepath.Join(ts.Getenv("WORK"), args[l-1])
					f, err := os.Create(outPath)
					ts.Check(err)
					defer f.Close()
					w = f
				}
				content := os.Expand(ts.ReadFile(args[0]), ts.Getenv)
				_, err := w.Write([]byte(content))
				ts.Check(err)
			},
			// kubectl runs kubectl with the namespace set to the test namespace
			"kubectl": func(ts *testscript.TestScript, neg bool, args []string) {
				err := ts.Exec("kubectl", append([]string{
					"--namespace", ts.Getenv("NAMESPACE"),
				}, args...)...)
				if !neg {
					ts.Check(err)
				} else if err == nil {
					ts.Fatalf("unexpected success")
				}
			},
			// atlas runs the atlas binary in the controller-manager pod
			"atlas": func(ts *testscript.TestScript, neg bool, args []string) {
				err := ts.Exec("kubectl", "exec",
					"-n", nsController, ts.Getenv("CONTROLLER"), "--", "sh", "-c",
					fmt.Sprintf("ATLAS_TOKEN=%s atlas %s", ts.Getenv("ATLAS_TOKEN"), strings.Join(args, " ")),
				)
				if !neg {
					ts.Check(err)
				} else if err == nil {
					ts.Fatalf("unexpected success")
				}
			},
		},
	})
}

func pipeFile(p string, fn func(w io.Writer) error) error {
	fs, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer fs.Close()
	return fn(fs)
}

func getProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.Replace(wd, "/test/e2e", "", -1)
	return wd, nil
}
