/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ariga/atlas-operator/controllers/internal/atlas"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

// AtlasSchemaReconciler reconciles a AtlasSchema object
type AtlasSchemaReconciler struct {
	client.Client
	CLI    *atlas.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *AtlasSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	sc := &dbv1alpha1.AtlasSchema{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	u, err := r.url(ctx, sc)
	if err != nil {
		return ctrl.Result{}, err
	}
	drv := driver(u.Scheme)
	if drv == "" {
		return ctrl.Result{}, fmt.Errorf("driver not found for scheme %q", u.Scheme)
	}
	// make sure we have a dev db running
	devDB := &v1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: req.Name + "-dev-db", Namespace: req.Namespace}, devDB)
	if apierrors.IsNotFound(err) {
		devDB, err = r.devDBDeployment(ctx, sc, drv)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{
			RequeueAfter: time.Second * 15,
		}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	devURL, err := r.devURL(ctx, req.Name, drv, u.Path != "")
	if err != nil {
		return ctrl.Result{}, err
	}
	log.Info("dev db address", "url", devURL)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasSchema{}).
		Complete(r)
}

func (r *AtlasSchemaReconciler) url(ctx context.Context, sch *dbv1alpha1.AtlasSchema) (*url.URL, error) {
	var us string
	switch s := sch.Spec; {
	case s.URL != "":
		us = s.URL
	case s.URLFrom.SecretKeyRef != nil:
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: sch.Namespace, Name: s.URLFrom.SecretKeyRef.Name}, secret); err != nil {
			return nil, err
		}
		us = string(secret.Data[s.URLFrom.SecretKeyRef.Key])
	default:
		return nil, errors.New("no url specified")
	}
	return url.Parse(us)
}

func (r *AtlasSchemaReconciler) devDBDeployment(ctx context.Context, sc *dbv1alpha1.AtlasSchema, driver string) (*v1.Deployment, error) {
	ls := labelsForDevDB(sc.Name, driver)
	d := &v1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-dev-db",
			Namespace: sc.Namespace,
		},
		Spec: v1.DeploymentSpec{
			Replicas: pointer.Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: pointer.Bool(true),
						RunAsUser:    pointer.Int64(1000),
					},
				},
			},
		},
	}
	container := corev1.Container{
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             pointer.Bool(true),
			RunAsUser:                pointer.Int64(1000),
			AllowPrivilegeEscalation: pointer.Bool(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{
					"ALL",
				},
			},
		},
	}
	switch {
	case strings.HasPrefix(driver, "mysql"):
		container.Name = "mysql"
		container.Image = "mysql:8"
		container.Env = []corev1.EnvVar{
			{
				Name:  "MYSQL_ROOT_PASSWORD",
				Value: "pass",
			},
			{
				Name:  "MYSQL_DATABASE",
				Value: "dev",
			},
		}
		container.Ports = []corev1.ContainerPort{
			{
				Name:          "mysql",
				ContainerPort: 3306,
			},
		}
	case strings.HasPrefix(driver, "postgres"):
		container.Name = "postgres"
		container.Image = "postgres:15"
		container.Env = []corev1.EnvVar{
			{
				Name:  "POSTGRES_PASSWORD",
				Value: "pass",
			},
			{
				Name:  "POSTGRES_DB",
				Value: "dev",
			},
			{
				Name:  "POSTGRES_USER",
				Value: "root",
			},
		}
		container.Ports = []corev1.ContainerPort{
			{
				Name:          "postgres",
				ContainerPort: 5432,
			},
		}
	default:
		return nil, fmt.Errorf("unsupported driver: %s", driver)
	}
	d.Spec.Template.Spec.Containers = []corev1.Container{container}
	if err := ctrl.SetControllerReference(sc, d, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// labelsForDevDB returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForDevDB(name, driver string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "atlas-dev-db",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/part-of":    "atlas-operator",
		"app.kubernetes.io/created-by": "controller-manager",
		"atlasgo.io/engine":            driver,
	}
}

func (r *AtlasSchemaReconciler) devURL(ctx context.Context, name, driver string, schemaScope bool) (string, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels(labelsForDevDB(name, driver))); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", errors.New("no pods found")
	}
	// iterate through the pods and find the one that is running
	var pod *corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			pod = &p
			break
		}
	}
	if pod == nil {
		return "", fmt.Errorf("no running pods found")
	}
	log.FromContext(ctx).Info("found running pod", "pod", pod)
	addr := &url.URL{
		Scheme: driver,
		User:   url.UserPassword("root", "pass"),
		Host:   fmt.Sprintf("%s:%d", pod.Status.PodIP, pod.Spec.Containers[0].Ports[0].ContainerPort),
	}
	if schemaScope {
		addr.Path = "dev"
	}
	return addr.String(), nil
}

func driver(scheme string) string {
	switch {
	case strings.HasPrefix(scheme, "mysql"):
		return "mysql"
	case strings.HasPrefix(scheme, "postgres"):
		return "postgres"
	default:
		return ""
	}
}
