// Copyright 2023 The Atlas Operator Authors.
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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

const (
	annoConnTmpl  = "atlasgo.io/conntmpl"
	labelEngine   = "atlasgo.io/engine"
	labelInstance = "app.kubernetes.io/instance"
)

const (
	ReasonCreatedDevDB  = "CreatedDevDB"
	ReasonCleanUpDevDB  = "CleanUpDevDB"
	ReasonScaledUpDevDB = "ScaledUpDevDB"
)

var defaultLabels = map[string]string{
	"app.kubernetes.io/name":       "atlas-dev-db",
	"app.kubernetes.io/part-of":    "atlas-operator",
	"app.kubernetes.io/created-by": "controller-manager",
}

type (
	// TODO: Refactor this to a separate controller
	devDBReconciler struct {
		client.Client
		scheme   *runtime.Scheme
		recorder record.EventRecorder
		prewarm  bool
	}
)

var errWaitDevDB = transient(errors.New("waiting for dev database to be ready"))

func newDevDB(mgr Manager, r record.EventRecorder, prewarm bool) *devDBReconciler {
	if r == nil {
		// Only create a new recorder if it is not provided.
		// This keep the controller from creating multiple recorders.
		r = mgr.GetEventRecorderFor("devdb")
	}
	return &devDBReconciler{
		Client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		recorder: r,
		prewarm:  prewarm,
	}
}

// cleanUp clean up any resources created by the controller
func (r *devDBReconciler) cleanUp(ctx context.Context, sc client.Object) {
	key := nameDevDB(sc)
	// If prewarmDevDB is false, scale down the deployment to 0
	if !r.prewarm {
		deploy := &appsv1.Deployment{}
		err := r.Get(ctx, key, deploy)
		if err != nil {
			r.recorder.Eventf(sc, corev1.EventTypeWarning, ReasonCleanUpDevDB, "Error getting devDB deployment: %v", err)
			return
		}
		deploy.Spec.Replicas = ptr.To[int32](0)
		if err := r.Update(ctx, deploy); err != nil {
			r.recorder.Eventf(sc, corev1.EventTypeWarning, ReasonCleanUpDevDB, "Error scaling down devDB deployment: %v", err)
		}
		return
	}
	// delete pods to clean up
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(map[string]string{
			labelInstance: key.Name,
		}),
	); err != nil {
		r.recorder.Eventf(sc, corev1.EventTypeWarning, ReasonCleanUpDevDB, "Error listing devDB pods: %v", err)
		return
	}
	for _, p := range pods.Items {
		if err := r.Delete(ctx, &p); err != nil {
			r.recorder.Eventf(sc, corev1.EventTypeWarning, ReasonCleanUpDevDB, "Error deleting devDB pod %s: %v", p.Name, err)
		}
	}
}

// devURL returns the URL of the dev database for the given target URL.
// It creates a dev database if it does not exist.
func (r *devDBReconciler) devURL(ctx context.Context, sc client.Object, targetURL url.URL, podSpec *corev1.PodSpec, devURL string) (string, error) {
	drv := dbv1alpha1.DriverBySchema(targetURL.Scheme)
	if drv == dbv1alpha1.DriverSQLite {
		return "sqlite://db?mode=memory", nil
	}
	// make sure we have a dev db running
	key := nameDevDB(sc)
	deploy := &appsv1.Deployment{}
	switch err := r.Get(ctx, key, deploy); {
	// The dev database already exists.
	case err == nil && (deploy.Spec.Replicas == nil || *deploy.Spec.Replicas == 0):
		// If it is scaled down, scale it up.
		deploy.Spec.Replicas = ptr.To[int32](1)
		if err := r.Update(ctx, deploy); err != nil {
			return "", transient(err)
		}
		r.recorder.Eventf(sc, corev1.EventTypeNormal, ReasonScaledUpDevDB, "Scaled up dev database deployment: %s", deploy.Name)
		return "", errWaitDevDB
	// The dev database does not exist, create it.
	case apierrors.IsNotFound(err):
		switch {
		case podSpec == nil:
			podSpec, devURL, err = automaticDevDBSpec(targetURL)
			if err != nil {
				return "", fmt.Errorf("failed to create dev database spec: %w", err)
			}
		case devURL == "":
			return "", fmt.Errorf("devURL is required when customDevDB is provided")
		}
		deploy = deploymentDevDB(key, drv, *podSpec, devURL)
		// Set the owner reference to the given object
		// This will ensure that the deployment is deleted when the owner is deleted.
		if err := ctrl.SetControllerReference(sc, deploy, r.scheme); err != nil {
			return "", err
		}
		if err := r.Create(ctx, deploy); err != nil {
			return "", transient(err)
		}
		r.recorder.Eventf(sc, corev1.EventTypeNormal, ReasonCreatedDevDB, "Created dev database deployment: %s", key.Name)
		return "", errWaitDevDB
	// An error occurred while getting the dev database.
	case err != nil:
		return "", transient(err)
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(map[string]string{
			labelEngine:   drv.String(),
			labelInstance: key.Name,
		}),
	); err != nil {
		return "", transient(err)
	}
	pod, err := readyPod(pods.Items)
	if err != nil {
		return "", transient(err)
	}
	if conn, ok := pod.Annotations[annoConnTmpl]; ok {
		u, err := url.Parse(conn)
		if err != nil {
			return "", fmt.Errorf("invalid connection template: %w", err)
		}
		if p := u.Port(); p != "" {
			u.Host = fmt.Sprintf("%s:%s", pod.Status.PodIP, p)
		} else {
			u.Host = pod.Status.PodIP
		}
		return u.String(), nil
	}
	// If the connection template is not found, there is an issue with
	// the pod spec and the error is not transient.
	return "", errors.New("no connection template annotation found")
}

func deploymentDevDB(key types.NamespacedName, drv dbv1alpha1.Driver, podSpec corev1.PodSpec, urlTemplate string) *appsv1.Deployment {
	labels := defaultLabels
	labels[labelEngine] = drv.String()
	labels[labelInstance] = key.Name
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						annoConnTmpl: urlTemplate,
					},
				},
				Spec: podSpec,
			},
		},
	}
}

// automaticDevDBSpec returns a PodSpec for a development database based on the target URL.
func automaticDevDBSpec(targetURL url.URL) (*corev1.PodSpec, string, error) {
	drv := dbv1alpha1.DriverBySchema(targetURL.Scheme)
	var (
		user string
		pass string
		path string
		q    = url.Values{}
	)
	c := corev1.Container{
		Name: drv.String(),
		StartupProbe: &corev1.Probe{
			FailureThreshold: 30,
			PeriodSeconds:    10,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
	switch drv {
	case dbv1alpha1.DriverPostgres:
		// URLs
		user, pass, path = "postgres", "pass", "postgres"
		q.Set("sslmode", "disable")
		if drv.SchemaBound(targetURL) {
			q.Set("search_path", "public")
		}
		// Containers
		c.Image = "postgres:latest"
		c.Ports = []corev1.ContainerPort{
			{Name: drv.String(), ContainerPort: 5432},
		}
		c.StartupProbe.Exec = &corev1.ExecAction{
			Command: []string{"pg_isready"},
		}
		c.Env = []corev1.EnvVar{
			{Name: "POSTGRES_DB", Value: path},
			{Name: "POSTGRES_USER", Value: user},
			{Name: "POSTGRES_PASSWORD", Value: pass},
		}
		c.SecurityContext.RunAsUser = ptr.To[int64](999)
	case dbv1alpha1.DriverSQLServer:
		// URLs
		user, pass, path = "sa", "P@ssw0rd0995", ""
		q.Set("database", "master")
		if !drv.SchemaBound(targetURL) {
			q.Set("mode", "DATABASE")
		}
		// Containers
		c.Image = "mcr.microsoft.com/mssql/server:2022-latest"
		c.Ports = []corev1.ContainerPort{
			{Name: drv.String(), ContainerPort: 1433},
		}
		c.StartupProbe.Exec = &corev1.ExecAction{
			Command: []string{
				"/opt/mssql-tools18/bin/sqlcmd",
				"-C", "-Q", "SELECT 1",
				"-U", user, "-P", pass,
			},
		}
		c.Env = []corev1.EnvVar{
			{Name: "MSSQL_SA_PASSWORD", Value: pass},
			{Name: "MSSQL_PID", Value: os.Getenv("MSSQL_PID")},
			{Name: "ACCEPT_EULA", Value: os.Getenv("MSSQL_ACCEPT_EULA")},
		}
		c.SecurityContext.RunAsUser = ptr.To[int64](10001)
		c.SecurityContext.Capabilities.Add = []corev1.Capability{
			// The --cap-add NET_BIND_SERVICE flag is required for non-root SQL Server
			// containers to allow `sqlservr` to bind the default MSDTC RPC on port `135`
			// which is less than 1024.
			"NET_BIND_SERVICE",
			// The --cap-add SYS_PTRACE flag is required for non-root SQL Server
			// containers to generate dumps for troubleshooting purposes.
			"SYS_PTRACE",
		}
	case dbv1alpha1.DriverMySQL:
		// URLs
		user, pass, path = "root", "pass", ""
		// Containers
		c.Image = "mysql:latest"
		c.Ports = []corev1.ContainerPort{
			{Name: drv.String(), ContainerPort: 3306},
		}
		c.StartupProbe.Exec = &corev1.ExecAction{
			Command: []string{
				"mysql",
				"-h", "127.0.0.1",
				"-e", "SELECT 1",
				"-u", user, "-p" + pass,
			},
		}
		c.Env = []corev1.EnvVar{
			{Name: "MYSQL_ROOT_PASSWORD", Value: pass},
		}
		if drv.SchemaBound(targetURL) {
			path = "dev"
			c.Env = append(c.Env, corev1.EnvVar{
				Name: "MYSQL_DATABASE", Value: path,
			})
		}
		c.SecurityContext.RunAsUser = ptr.To[int64](1000)
	case dbv1alpha1.DriverMariaDB:
		// URLs
		user, pass, path = "root", "pass", ""
		// Containers
		c.Image = "mariadb:latest"
		c.Ports = []corev1.ContainerPort{
			{Name: drv.String(), ContainerPort: 3306},
		}
		c.StartupProbe.Exec = &corev1.ExecAction{
			Command: []string{
				"mariadb",
				"-h", "127.0.0.1",
				"-e", "SELECT 1",
				"-u", user, "-p" + pass,
			},
		}
		c.Env = []corev1.EnvVar{
			{Name: "MARIADB_ROOT_PASSWORD", Value: pass},
		}
		if drv.SchemaBound(targetURL) {
			path = "dev"
			c.Env = append(c.Env, corev1.EnvVar{
				Name: "MARIADB_DATABASE", Value: path,
			})
		}
		c.SecurityContext.RunAsUser = ptr.To[int64](999)
	case dbv1alpha1.DriverClickHouse:
		// URLs
		user, pass, path = "root", "pass", ""
		// Containers
		c.Image = "clickhouse/clickhouse-server:latest"
		c.Ports = []corev1.ContainerPort{
			{Name: drv.String(), ContainerPort: 9000},
		}
		c.StartupProbe.Exec = &corev1.ExecAction{
			Command: []string{
				"clickhouse-client", "-q", "SELECT 1",
			},
		}
		c.Env = []corev1.EnvVar{
			{Name: "CLICKHOUSE_USER", Value: user},
			{Name: "CLICKHOUSE_PASSWORD", Value: pass},
		}
		if drv.SchemaBound(targetURL) {
			path = "dev"
			c.Env = append(c.Env, corev1.EnvVar{
				Name: "CLICKHOUSE_DB", Value: path,
			})
		}
		c.SecurityContext.RunAsUser = ptr.To[int64](101)
		c.SecurityContext.Capabilities.Add = []corev1.Capability{
			"SYS_NICE", "NET_ADMIN", "IPC_LOCK",
		}
	default:
		return nil, "", fmt.Errorf(`devdb: unsupported driver %q. You need to provide the devURL on the resource: https://atlasgo.io/integrations/kubernetes/operator#devurl`, drv)
	}
	conn := &url.URL{
		Scheme:   c.Ports[0].Name,
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("localhost:%d", c.Ports[0].ContainerPort),
		Path:     path,
		RawQuery: q.Encode(),
	}
	return &corev1.PodSpec{
		Containers: []corev1.Container{c},
	}, conn.String(), nil
}

func readyPod(pods []corev1.Pod) (*corev1.Pod, error) {
	if len(pods) == 0 {
		return nil, errors.New("no pods found")
	}
	idx := slices.IndexFunc(pods, func(p corev1.Pod) bool {
		return slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
			return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		})
	})
	if idx == -1 {
		return nil, errors.New("no running pods found")
	}
	return &pods[idx], nil
}

// nameDevDB returns the namespaced name of the dev database.
func nameDevDB(owner metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Name:      fmt.Sprintf("%s-atlas-dev-db", owner.GetName()),
		Namespace: owner.GetNamespace(),
	}
}
