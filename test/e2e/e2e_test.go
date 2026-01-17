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
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	nsController = "atlas-operator-system"
)

func TestOperator(t *testing.T) {
	require.NoError(t, os.MkdirAll("logs", 0755))
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster == "" {
		kindCluster = "kind"
	}
	// Creating kubeconfig for the kind cluster
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, pipeFile(0600, kubeconfig, func(f io.Writer) error {
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
	_, err = kind("skaffold", "run", "--wait-for-connection=true")
	require.NoError(t, err)
	var controllerPod string
	for range 10 {
		// Getting the controller-manager pod name
		output, err := kind("kubectl", "get", "pod",
			"-n", nsController,
			"-l", "control-plane=controller-manager",
			"-o", "jsonpath",
			"--template", "{.items[*].metadata.name}",
		)
		require.NoError(t, err)
		pods := strings.Split(output, " ")
		if len(pods) == 1 && pods[0] != "" {
			controllerPod = pods[0]
			break
		}
		// Wait 5s before retrying
		<-time.After(time.Second * 5)
	}
	require.NotEmpty(t, controllerPod, "controller-manager pod not found")
	t.Cleanup(func() {
		logs, err := kind("kubectl", "logs", "-n", nsController, controllerPod)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("logs/controller.txt", []byte(logs), 0644))
		_, err = kind("skaffold", "delete")
		require.NoError(t, err)
	})
	// Running the test script
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("testscript"),
		Setup: func(e *testscript.Env) (err error) {
			e.Setenv("CONTROLLER_NS", nsController)
			e.Setenv("CONTROLLER", controllerPod)
			// Sharing the atlas token with the test
			e.Setenv("ATLAS_TOKEN", os.Getenv("ATLAS_TOKEN"))
			// Ensure the test in running in the right kube context
			e.Setenv("KUBECONFIG", kubeconfig)
			// Creating a namespace for the test
			ns := fmt.Sprintf("e2e-%s-%d", path.Base(e.WorkDir), time.Now().UnixMicro())
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
			// db deploys a database and exports its URL in the requested environment variable.
			"db": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg {
					ts.Fatalf("unsupported: ! db")
				}
				var res *devDBResources
				var schemaBound bool
				var name string
				switch len(args) {
				case 4:
					mode := strings.ToLower(args[3])
					schemaBound = mode != "database"
					fallthrough
				case 3:
					override := strings.ToLower(args[2])
					if errs := validation.IsDNS1123Label(override); len(errs) > 0 {
						ts.Fatalf("db: invalid name %q: %s", args[2], strings.Join(errs, ", "))
					}
					name = override
					fallthrough
				case 2:
					drv := v1alpha1.Driver(strings.ToLower(args[0]))
					if name == "" {
						name = drv.String()
					}
					switch podSpec, devURL, err := controller.AutomaticDevDBSpec(drv, schemaBound, ts.Getenv); {
					case err != nil:
						ts.Fatalf("db: %v", err)
					case podSpec == nil:
						ts.Fatalf("db: %v", fmt.Errorf("db: driver %s returned no pod spec", drv))
					default:
						podSpec.Containers[0].Name = name
						res = &devDBResources{
							deployment: controller.DeploymentDevDB(types.NamespacedName{
								Name: name,
							}, drv, *podSpec, devURL),
							template: devURL,
						}
					}
				default:
					ts.Fatalf("usage: db <driver> <env> [name] [mode]")
				}
				manifestPath, err := res.writeManifest(ts.Getenv("WORK"))
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				ts.Defer(func() {
					_ = os.Remove(manifestPath)
				})
				ns := ts.Getenv("NAMESPACE")
				ts.Check(ts.Exec("kubectl", "-n", ns, "apply", "-f", manifestPath))
				ts.Check(ts.Exec("kubectl", "-n", ns, "wait", "--for=condition=available", "--timeout=2h", "deployment/"+name))
				ts.Check(ts.Exec("kubectl", "-n", ns, "wait", "--for=condition=ready", "--timeout=2h", "pod", "-l", "app.kubernetes.io/instance="+name))
				podIP, err := kind("kubectl", "-n", ns, "get", "pod", "-l", "app.kubernetes.io/instance="+name, "-o", "jsonpath={.items[0].status.podIP}")
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				dsn, err := res.DSN(podIP)
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				ts.Setenv(args[1], dsn)
			},
			// atlas runs the atlas binary in the controller-manager pod
			"atlas": func(ts *testscript.TestScript, neg bool, args []string) {
				quotedArgs := make([]string, 0, len(args))
				for _, a := range args {
					quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
				}
				err := ts.Exec("kubectl", "exec",
					"-n", nsController, ts.Getenv("CONTROLLER"), "--", "sh", "-c",
					fmt.Sprintf("ATLAS_TOKEN=%s atlas %s", ts.Getenv("ATLAS_TOKEN"), strings.Join(quotedArgs, " ")),
				)
				if !neg {
					ts.Check(err)
				} else if err == nil {
					ts.Fatalf("unexpected success")
				}
			},
			// helm runs helm with the namespace set to the test namespace
			"helm": func(ts *testscript.TestScript, neg bool, args []string) {
				err := ts.Exec("helm", append([]string{"-n", ts.Getenv("NAMESPACE")}, args...)...)
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
			// kubectl-wait-ready runs kubectl wait for the given resource to be ready
			"kubectl-wait-ready": func(ts *testscript.TestScript, neg bool, args []string) {
				if len(args) == 0 {
					ts.Fatalf("usage: kubectl-wait-ready <resource> <name>")
				}
				err := ts.Exec("kubectl", append([]string{"-n", ts.Getenv("NAMESPACE"),
					"wait", "--for=condition=ready", "--timeout=2h",
				}, args...)...)
				if !neg {
					ts.Check(err)
				} else if err == nil {
					ts.Fatalf("unexpected success")
				}
			},
			// kubectl-wait-available runs kubectl wait for the given deployment available
			"kubectl-wait-available": func(ts *testscript.TestScript, neg bool, args []string) {
				if len(args) == 0 {
					ts.Fatalf("usage: kubectl-wait-available <name>")
				}
				// wait for the deployment to be available,
				// minimum available is 1 so pods are created
				err = ts.Exec("kubectl", append([]string{"-n", ts.Getenv("NAMESPACE"),
					"wait", "--for=condition=available", "--timeout=2h",
				}, args...)...)
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
			// plans-rm removes the plans from the given file
			"plans-rm": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg {
					ts.Fatalf("unsupported: ! plans-rm")
				}
				if len(args) < 1 {
					ts.Fatalf("usage: plans-rm filename")
				}
				for p := range strings.SplitSeq(ts.ReadFile(args[0]), "\n") {
					if p == "" {
						continue
					}
					ts.Check(ts.Exec("kubectl", "exec",
						"-n", nsController, ts.Getenv("CONTROLLER"), "--", "sh", "-c",
						fmt.Sprintf("ATLAS_TOKEN=%s atlas schema plan rm --url=%s", ts.Getenv("ATLAS_TOKEN"), p),
					))
				}
			},
		},
	})
}

type devDBResources struct {
	deployment *appsv1.Deployment
	template   string
}

func (r devDBResources) DSN(podIP string) (string, error) {
	podIP = strings.TrimSpace(podIP)
	if podIP == "" {
		return "", fmt.Errorf("empty pod IP")
	}
	if r.template == "" {
		return "", nil
	}
	u, err := url.Parse(r.template)
	if err != nil {
		return "", err
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(podIP, port)
	} else {
		u.Host = podIP
	}
	return u.String(), nil
}

func (r devDBResources) writeManifest(dir string) (string, error) {
	manifest, err := yaml.Marshal(r.deployment)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "db-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(manifest); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func pipeFile(perm fs.FileMode, p string, fn func(w io.Writer) error) error {
	fs, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, perm)
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
