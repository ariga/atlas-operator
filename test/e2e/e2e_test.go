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
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
		<-time.After(5 * time.Second)
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
				if len(args) < 2 || len(args) > 3 {
					ts.Fatalf("usage: db <image> <env> [name]")
				}
				image := args[0]
				envVar := args[1]
				cfg, err := newDBConfig(image)
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				if len(args) == 3 {
					name := strings.ToLower(args[2])
					if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
						ts.Fatalf("db: invalid name %q: %s", args[2], strings.Join(errs, ", "))
					}
					cfg.name = name
				}
				manifestYAML, err := cfg.manifest(image)
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				manifestPath, err := writeManifest(ts.Getenv("WORK"), manifestYAML)
				if err != nil {
					ts.Fatalf("db: %v", err)
				}
				ts.Defer(func() {
					_ = os.Remove(manifestPath)
				})
				ns := ts.Getenv("NAMESPACE")
				ts.Check(ts.Exec("kubectl", "-n", ns, "apply", "-f", manifestPath))
				ts.Check(ts.Exec("kubectl", "-n", ns, "wait", "--for=condition=available", "--timeout=2h", "deployment/"+cfg.name))
				ts.Check(ts.Exec("kubectl", "-n", ns, "wait", "--for=condition=ready", "--timeout=2h", "pod", "-l", "app="+cfg.name))
				ts.Setenv(envVar, cfg.dsn(ns))
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

type (
	dbConfig struct {
		name         string
		scheme       string
		port         int
		database     string
		user         string
		pass         string
		query        url.Values
		env          []dbEnvVar
		args         []string
		startupCmd   []string
		readinessCmd []string
	}
	dbEnvVar struct {
		Name  string
		Value string
	}
)

func newDBConfig(image string) (*dbConfig, error) {
	img := strings.ToLower(image)
	switch {
	case strings.Contains(img, "mariadb"):
		return &dbConfig{
			name:     "mariadb",
			scheme:   "mariadb",
			port:     3306,
			database: "myapp",
			user:     "root",
			pass:     "pass",
			env: []dbEnvVar{
				{Name: "MARIADB_ROOT_PASSWORD", Value: "pass"},
				{Name: "MARIADB_DATABASE", Value: "myapp"},
			},
			startupCmd:   []string{"mariadb", "-ppass", "-h", "127.0.0.1", "-e", "SELECT 1"},
			readinessCmd: []string{"mariadb", "-ppass", "-h", "127.0.0.1", "-e", "SELECT 1"},
		}, nil
	case strings.Contains(img, "mysql"):
		return &dbConfig{
			name:     "mysql",
			scheme:   "mysql",
			port:     3306,
			database: "myapp",
			user:     "root",
			pass:     "pass",
			env: []dbEnvVar{
				{Name: "MYSQL_ROOT_PASSWORD", Value: "pass"},
				{Name: "MYSQL_DATABASE", Value: "myapp"},
			},
			startupCmd:   []string{"mysql", "-ppass", "-h", "127.0.0.1", "-e", "SELECT 1"},
			readinessCmd: []string{"mysql", "-ppass", "-h", "127.0.0.1", "-e", "SELECT 1"},
		}, nil
	case strings.Contains(img, "postgres"):
		return &dbConfig{
			name:     "postgres",
			scheme:   "postgres",
			port:     5432,
			database: "postgres",
			user:     "root",
			pass:     "pass",
			query: url.Values{
				"sslmode": {"disable"},
			},
			env: []dbEnvVar{
				{Name: "POSTGRES_DB", Value: "postgres"},
				{Name: "POSTGRES_USER", Value: "root"},
				{Name: "POSTGRES_PASSWORD", Value: "pass"},
			},
			startupCmd:   []string{"pg_isready"},
			readinessCmd: []string{"pg_isready", "--username=root", "--dbname=postgres"},
		}, nil
	case strings.Contains(img, "clickhouse"):
		return &dbConfig{
			name:     "clickhouse",
			scheme:   "clickhouse",
			port:     9000,
			database: "myapp",
			user:     "root",
			pass:     "pass",
			env: []dbEnvVar{
				{Name: "CLICKHOUSE_USER", Value: "root"},
				{Name: "CLICKHOUSE_PASSWORD", Value: "pass"},
				{Name: "CLICKHOUSE_DB", Value: "myapp"},
			},
			startupCmd:   []string{"clickhouse-client", "-q", "SELECT 1"},
			readinessCmd: []string{"clickhouse-client", "-q", "SELECT 1"},
		}, nil
	case strings.Contains(img, "cockroach"):
		return &dbConfig{
			name:     "cockroachdb",
			scheme:   "crdb",
			port:     26257,
			database: "defaultdb",
			user:     "root",
			pass:     "pass",
			query: url.Values{
				"sslmode": {"disable"},
			},
			args:         []string{"start-single-node", "--insecure"},
			startupCmd:   []string{"cockroach", "sql", "--insecure", "-e", "SELECT 1"},
			readinessCmd: []string{"cockroach", "sql", "--insecure", "-e", "SELECT 1"},
		}, nil
	case strings.Contains(img, "mssql") || strings.Contains(img, "sqlserver"):
		return &dbConfig{
			name:   "sqlserver",
			scheme: "sqlserver",
			port:   1433,
			user:   "sa",
			pass:   "P@ssw0rd0995",
			query: url.Values{
				"database": {"master"},
			},
			env: []dbEnvVar{
				{Name: "ACCEPT_EULA", Value: "Y"},
				{Name: "MSSQL_PID", Value: "Developer"},
				{Name: "MSSQL_SA_PASSWORD", Value: "P@ssw0rd0995"},
			},
			startupCmd:   []string{"/opt/mssql-tools18/bin/sqlcmd", "-C", "-U", "sa", "-P", "P@ssw0rd0995", "-Q", "SELECT 1"},
			readinessCmd: []string{"/opt/mssql-tools18/bin/sqlcmd", "-C", "-U", "sa", "-P", "P@ssw0rd0995", "-Q", "SELECT 1"},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported database image %q", image)
	}
}

func (cfg *dbConfig) manifest(image string) ([]byte, error) {
	selector := map[string]string{"app": cfg.name}
	labels := map[string]string{"app": cfg.name}
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.name,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{
				{
					Name:       cfg.scheme,
					Port:       int32(cfg.port),
					TargetPort: intstr.FromString(cfg.scheme),
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	replicas := int32(1)
	container := corev1.Container{
		Name:  cfg.name,
		Image: image,
		Ports: []corev1.ContainerPort{
			{
				Name:          cfg.scheme,
				ContainerPort: int32(cfg.port),
			},
		},
	}
	if len(cfg.args) > 0 {
		container.Args = cfg.args
	}
	if len(cfg.env) > 0 {
		envs := make([]corev1.EnvVar, 0, len(cfg.env))
		for _, env := range cfg.env {
			envs = append(envs, corev1.EnvVar{Name: env.Name, Value: env.Value})
		}
		container.Env = envs
	}
	if len(cfg.startupCmd) > 0 {
		container.StartupProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: cfg.startupCmd},
			},
			FailureThreshold: 30,
			PeriodSeconds:    10,
		}
	}
	if len(cfg.readinessCmd) > 0 {
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: cfg.readinessCmd},
			},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		}
	}
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	}
	svcYAML, err := yaml.Marshal(svc)
	if err != nil {
		return nil, err
	}
	depYAML, err := yaml.Marshal(dep)
	if err != nil {
		return nil, err
	}
	manifest := append(svcYAML, []byte("---\n")...)
	manifest = append(manifest, depYAML...)
	return manifest, nil
}

func (cfg *dbConfig) dsn(namespace string) string {
	u := &url.URL{
		Scheme: cfg.scheme,
		User:   url.UserPassword(cfg.user, cfg.pass),
		Host:   fmt.Sprintf("%s.%s:%d", cfg.name, namespace, cfg.port),
	}
	if cfg.database != "" {
		u.Path = cfg.database
	}
	if cfg.query != nil {
		u.RawQuery = cfg.query.Encode()
	}
	return u.String()
}

func writeManifest(dir string, content []byte) (string, error) {
	f, err := os.CreateTemp(dir, "db-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
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
