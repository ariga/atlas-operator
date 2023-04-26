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
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/atlas"
	"golang.org/x/exp/slices"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	devDBSuffix = "-atlas-dev-db"
	hostReplace = "REPLACE_HOST"
)

var (
	//go:embed devdb.tmpl
	devDBTmpl string
	tmpl      *template.Template
)

type (
	// AtlasSchemaReconciler reconciles a AtlasSchema object
	AtlasSchemaReconciler struct {
		client.Client
		CLI    *atlas.Client
		Scheme *runtime.Scheme
	}
	// devDB contains values used to render a devDB pod template.
	devDB struct {
		Name        string
		Namespace   string
		SchemaBound bool
		Driver      string
		DB          string
		Port        int
		UID         int
	}
)

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
	err = r.Get(ctx, types.NamespacedName{Name: req.Name + devDBSuffix, Namespace: req.Namespace}, devDB)
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
	devURL, err := r.devURL(ctx, req.Name, drv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, u.String(), devURL, sc); err != nil {
		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	}
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
	d := &v1.Deployment{}
	v := devDB{
		Name:      sc.Name + devDBSuffix,
		Namespace: sc.Namespace,
		Driver:    driver,
		UID:       1000,
	}
	switch driver {
	case "postgres":
		v.Port = 5432
		v.UID = 999
		v.DB = "postgres"
	case "mysql":
		v.Port = 3306
		v.DB = "dev"
		v.SchemaBound = true
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, &v); err != nil {
		return nil, err
	}
	if err := yaml.NewYAMLToJSONDecoder(&b).Decode(d); err != nil {
		return nil, err
	}
	if err := ctrl.SetControllerReference(sc, d, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (r *AtlasSchemaReconciler) devURL(ctx context.Context, name, driver string) (string, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels(map[string]string{
		"app.kubernetes.io/instance": name + devDBSuffix,
		"atlasgo.io/engine":          driver,
	})); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", errors.New("no pods found")
	}
	idx := slices.IndexFunc(pods.Items, func(p corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning
	})
	if idx == -1 {
		return "", errors.New("no running pods found")
	}
	pod := pods.Items[idx]
	ct, ok := pod.Annotations["atlasgo.io/conntmpl"]
	if !ok {
		return "", errors.New("no connection template label found")
	}
	return strings.ReplaceAll(
		ct,
		hostReplace,
		fmt.Sprintf("%s:%d", pod.Status.PodIP, pod.Spec.Containers[0].Ports[0].ContainerPort),
	), nil
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

func (r *AtlasSchemaReconciler) apply(ctx context.Context, url, devURL string, tgt *dbv1alpha1.AtlasSchema) error {
	var desired, ext string
	switch sch := tgt.Spec.Schema; {
	case sch.HCL != "":
		desired = sch.HCL
		ext = "hcl"
	case sch.SQL != "":
		desired = sch.SQL
		ext = "sql"
	default:
		return fmt.Errorf("no schema specified")
	}
	file, clean, err := atlas.TempFile(desired, ext)
	if err != nil {
		return err
	}
	defer clean()
	_, err = r.CLI.SchemaApply(ctx, &atlas.SchemaApplyParams{
		URL:    url,
		To:     file,
		DevURL: devURL,
	})
	if err != nil {
		return err
	}
	return nil
}

func init() {
	var err error
	tmpl, err = template.New("devdb").Parse(devDBTmpl)
	if err != nil {
		panic(err)
	}
}

func (d *devDB) ConnTmpl() string {
	u := url.URL{
		Scheme: d.Driver,
		User:   url.UserPassword("root", "pass"),
		Host:   hostReplace,
		Path:   d.DB,
	}
	if q := u.Query(); d.Driver == "postgres" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	return u.String()
}
