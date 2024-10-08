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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
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

type (
	// TODO: Refactor this to a separate controller
	devDBReconciler struct {
		client.Client
		scheme   *runtime.Scheme
		recorder record.EventRecorder
		prewarm  bool
	}
)

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
			r.recorder.Eventf(sc, corev1.EventTypeWarning, "CleanUpDevDB", "Error getting devDB deployment: %v", err)
			return
		}
		deploy.Spec.Replicas = new(int32)
		if err := r.Update(ctx, deploy); err != nil {
			r.recorder.Eventf(sc, corev1.EventTypeWarning, "CleanUpDevDB", "Error scaling down devDB deployment: %v", err)
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
		r.recorder.Eventf(sc, corev1.EventTypeWarning, "CleanUpDevDB", "Error listing devDB pods: %v", err)
		return
	}
	for _, p := range pods.Items {
		if err := r.Delete(ctx, &p); err != nil {
			r.recorder.Eventf(sc, corev1.EventTypeWarning, "CleanUpDevDB", "Error deleting devDB pod %s: %v", p.Name, err)
		}
	}
}

// devURL returns the URL of the dev database for the given target URL.
// It creates a dev database if it does not exist.
func (r *devDBReconciler) devURL(ctx context.Context, sc client.Object, targetURL url.URL) (string, error) {
	drv := dbv1alpha1.DriverBySchema(targetURL.Scheme)
	if drv == "sqlite" {
		return "sqlite://db?mode=memory", nil
	}
	// make sure we have a dev db running
	key := nameDevDB(sc)
	deploy := &appsv1.Deployment{}
	switch err := r.Get(ctx, key, deploy); {
	case err == nil:
		// The dev database already exists,
		// If it is scaled down, scale it up.
		if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas == 0 {
			deploy.Spec.Replicas = ptr.To[int32](1)
			if err := r.Update(ctx, deploy); err != nil {
				return "", transient(err)
			}
			r.recorder.Eventf(sc, corev1.EventTypeNormal, "ScaledUpDevDB", "Scaled up dev database deployment: %s", deploy.Name)
			return "", transientAfter(errors.New("waiting for dev database to be ready"), 15*time.Second)
		}
	case apierrors.IsNotFound(err):
		// The dev database does not exist, create it.
		deploy, err := deploymentDevDB(key, drv, isSchemaBound(drv, &targetURL))
		if err != nil {
			return "", err
		}
		// Set the owner reference to the given object
		// This will ensure that the deployment is deleted when the owner is deleted.
		if err := ctrl.SetControllerReference(sc, deploy, r.scheme); err != nil {
			return "", err
		}
		if err := r.Create(ctx, deploy); err != nil {
			return "", transient(err)
		}
		r.recorder.Eventf(sc, corev1.EventTypeNormal, "CreatedDevDB", "Created dev database deployment: %s", deploy.Name)
		return "", transientAfter(errors.New("waiting for dev database to be ready"), 15*time.Second)
	default:
		// An error occurred while getting the dev database,
		return "", err
	}
	pods := &corev1.PodList{}
	switch err := r.List(ctx, pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(map[string]string{
			labelEngine:   drv,
			labelInstance: key.Name,
		}),
	); {
	case err != nil:
		return "", transient(err)
	case len(pods.Items) == 0:
		return "", transient(errors.New("no pods found"))
	}
	idx := slices.IndexFunc(pods.Items, isPodReady)
	if idx == -1 {
		return "", transient(errors.New("no running pods found"))
	}
	pod := pods.Items[idx]
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

func isPodReady(pod corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// devDB contains values used to render a devDB pod template.
type devDB struct {
	types.NamespacedName
	Driver      string
	User        string
	Pass        string
	Port        int
	UID         int
	DB          string
	SchemaBound bool
}

// connTmpl returns a connection template for the devDB.
func (d *devDB) connTmpl() string {
	u := url.URL{
		Scheme: d.Driver,
		User:   url.UserPassword(d.User, d.Pass),
		Host:   fmt.Sprintf("localhost:%d", d.Port),
		Path:   d.DB,
	}
	q := u.Query()
	switch {
	case d.Driver == "postgres":
		q.Set("sslmode", "disable")
	case d.Driver == "sqlserver":
		q.Set("database", d.DB)
		if !d.SchemaBound {
			q.Set("mode", "DATABASE")
		}
	}
	u.RawQuery = q.Encode()

	return u.String()
}

func (d *devDB) render(w io.Writer) error {
	return tmpl.ExecuteTemplate(w, "devdb.tmpl", d)
}

// deploymentDevDB returns a deployment for a dev database.
func deploymentDevDB(name types.NamespacedName, drv string, schemaBound bool) (*appsv1.Deployment, error) {
	v := &devDB{
		Driver:         drv,
		NamespacedName: name,
		User:           "root",
		Pass:           "pass",
		SchemaBound:    schemaBound,
	}
	switch drv {
	case "postgres":
		v.DB = "postgres"
		v.Port = 5432
		v.UID = 999
	case "mysql":
		if schemaBound {
			v.DB = "dev"
		}
		v.Port = 3306
		v.UID = 1000
	case "sqlserver":
		v.User = "sa"
		v.Pass = "P@ssw0rd0995"
		v.DB = "master"
		v.Port = 1433
	default:
		return nil, fmt.Errorf("unsupported driver %q", v.Driver)
	}
	b := &bytes.Buffer{}
	if err := v.render(b); err != nil {
		return nil, err
	}
	d := &appsv1.Deployment{}
	if err := yaml.NewYAMLToJSONDecoder(b).Decode(d); err != nil {
		return nil, err
	}
	d.Spec.Template.Annotations = map[string]string{
		annoConnTmpl: v.connTmpl(),
	}
	if drv == "sqlserver" {
		c := &d.Spec.Template.Spec.Containers[0]
		if v := os.Getenv("MSSQL_ACCEPT_EULA"); v != "" {
			c.Env = append(c.Env, corev1.EnvVar{
				Name:  "ACCEPT_EULA",
				Value: v,
			})
		}
		if v := os.Getenv("MSSQL_PID"); v != "" {
			c.Env = append(c.Env, corev1.EnvVar{
				Name:  "MSSQL_PID",
				Value: v,
			})
		}
	}
	return d, nil
}

// nameDevDB returns the namespaced name of the dev database.
func nameDevDB(owner metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Name:      fmt.Sprintf("%s-atlas-dev-db", owner.GetName()),
		Namespace: owner.GetNamespace(),
	}
}

// isSchemaBound returns true if the given target URL is schema bound.
// e.g. sqlite, postgres with search_path, mysql with path
func isSchemaBound(drv string, u *url.URL) bool {
	switch drv {
	case "sqlite":
		return true
	case "postgres":
		return u.Query().Get("search_path") != ""
	case "mysql":
		return u.Path != ""
	case "sqlserver":
		m := u.Query().Get("mode")
		return m == "" || strings.ToLower(m) == "schema"
	}
	return false
}
