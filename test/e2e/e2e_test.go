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
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster == "" {
		kindCluster = "kind"
	}
	// Creating kubeconfig for the kind cluster
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, pipeFile(kubeconfig, func(f io.Writer) error {
		cmd := exec.Command("kind", "get", "kubeconfig", "--name", kindCluster)
		cmd.Stdout, cmd.Stderr = f, os.Stderr
		return cmd.Run()
	}))
	dir, err := getProjectDir()
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
	_, err = kind("skaffold", "run", "--wait-for-connection=true", "-p", "integration")
	require.NoError(t, err)
	// Installing the CRDs
	_, err = kind("make", "install")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err = kind("make", "undeploy", "ignore-not-found=true")
		require.NoError(t, err)
	})
	// Accept the EULA and set the PID
	_, err = kind("kubectl", "set", "env",
		"-n", nsController, controller,
		"MSSQL_ACCEPT_EULA=Y", "MSSQL_PID=Developer")
	require.NoError(t, err)
	var controllerPod string
	for range 5 {
		// Getting the controller-manager pod name
		output, err := kind("kubectl", "get", "pod",
			"-n", nsController,
			"-l", "control-plane=controller-manager",
			"-o", "jsonpath",
			"--template", "{.items[*].metadata.name}",
		)
		require.NoError(t, err)
		pods := strings.Split(output, " ")
		if len(pods) == 1 {
			controllerPod = pods[0]
			break
		}
		// Wait 5s before retrying
		<-time.After(time.Second * 5)
	}
	require.NotEmpty(t, controllerPod, "controller-manager pod not found")
	// Running the test script
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("testdata", "atlas-schema"),
		Setup: func(e *testscript.Env) (err error) {
			e.Setenv("CONTROLLER_NS", nsController)
			e.Setenv("CONTROLLER", controllerPod)
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
			// kubectl runs kubectl with the namespace set to the test namespace
			"kubectl": func(ts *testscript.TestScript, neg bool, args []string) {
				err := ts.Exec("kubectl", append([]string{"-n", ts.Getenv("NAMESPACE")}, args...)...)
				if !neg {
					ts.Check(err)
				} else if err == nil {
					ts.Fatalf("unexpected success")
				}
			},
			// envfile read the file and using its content as environment variables
			"envfile": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg {
					ts.Fatalf("unsupported: ! envfile")
				}
				for _, k := range args {
					vals := strings.SplitN(k, "=", 2)
					if len(vals) != 2 {
						ts.Fatalf("expect KEY=filename, got %q", k)
					}
					ts.Setenv(vals[0], ts.ReadFile(vals[1]))
				}
			},
			// cat reads a file and expands it with the environment variables
			"cat": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg {
					ts.Fatalf("unsupported: ! cat")
				}
				if len(args) < 1 {
					ts.Fatalf("usage: cat filename")
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
