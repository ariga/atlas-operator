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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"ariga.io/atlas/sql/sqlcheck"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/atlas"
	"golang.org/x/exp/slices"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	devDBSuffix     = "-atlas-dev-db"
	hostReplace     = "REPLACE_HOST"
	schemaReadyCond = "Ready"
)

var (
	//go:embed devdb.tmpl
	devDBTmpl string
	tmpl      = template.Must(template.New("devdb").Parse(devDBTmpl))
)

type (
	// AtlasSchemaReconciler reconciles a AtlasSchema object
	AtlasSchemaReconciler struct {
		client.Client
		CLI    CLI
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
	// managed contains information about the managed database and its desired state.
	managed struct {
		ext    string
		schema string
		driver string
		url    *url.URL
	}
	CLI interface {
		SchemaApply(context.Context, *atlas.SchemaApplyParams) (*atlas.SchemaApply, error)
		SchemaInspect(ctx context.Context, data *atlas.SchemaInspectParams) (string, error)
		Lint(ctx context.Context, data *atlas.LintParams) (*atlas.SummaryReport, error)
	}
	destructiveErr struct {
		diags []sqlcheck.Diagnostic
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
	log := log.FromContext(ctx)
	var (
		sc      = &dbv1alpha1.AtlasSchema{}
		managed *managed
		err     error
	)
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if err := r.Status().Update(ctx, sc); err != nil {
			log.Error(err, "failed to update status")
		}
	}()
	// When the resource is first created, create the "Ready" condition.
	if sc.Status.Conditions == nil || len(sc.Status.Conditions) == 0 {
		setNotReady(sc, "Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	managed, err = r.extractManaged(ctx, sc)
	if err != nil {
		setNotReady(sc, "ReadSchema", err.Error())
		return ctrl.Result{}, err
	}
	// If the schema has changed and the schema's ready condition is not false, immediately set it to false.
	// This is done so that the observed status of the schema reflects its "in-progress" state while it is being
	// reconciled.
	if !meta.IsStatusConditionFalse(sc.Status.Conditions, schemaReadyCond) && managed.hash() != sc.Status.ObservedHash {
		setNotReady(sc, "Reconciling", "current schema does not match last applied managed")
		return ctrl.Result{Requeue: true}, nil
	}
	// make sure we have a dev db running
	devDB := &v1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: req.Name + devDBSuffix, Namespace: req.Namespace}, devDB)
	if apierrors.IsNotFound(err) {
		devDB, err = r.devDBDeployment(ctx, sc, managed.driver)
		if err != nil {
			setNotReady(sc, "CreatingDevDB", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{
			RequeueAfter: time.Second * 15,
		}, nil
	}
	if err != nil {
		setNotReady(sc, "GettingDevDB", err.Error())
		return ctrl.Result{}, err
	}
	devURL, err := r.devURL(ctx, req.Name, managed.driver)
	if err != nil {
		setNotReady(sc, "GettingDevDBURL", err.Error())
		return ctrl.Result{}, err
	}
	// Verify the first run doesn't contain destructive changes.
	if sc.Status.LastApplied == 0 {
		if err := r.verifyFirstRun(ctx, managed, devURL); err != nil {
			reason := "VerifyingFirstRun"
			var d destructiveErr
			if errors.As(err, &d) {
				reason = "FirstRunDestructive"
			}
			setNotReady(sc, reason, err.Error())
			return ctrl.Result{}, err
		}
	}
	app, err := r.apply(ctx, managed, devURL)
	if err != nil {
		setNotReady(sc, "ApplyingSchema", err.Error())
		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	}
	setReady(sc, managed, app)
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

func (r *AtlasSchemaReconciler) apply(ctx context.Context, des *managed, devURL string) (*atlas.SchemaApply, error) {
	file, clean, err := atlas.TempFile(des.schema, des.ext)
	if err != nil {
		return nil, err
	}
	defer clean()
	return r.CLI.SchemaApply(ctx, &atlas.SchemaApplyParams{
		URL:    des.url.String(),
		To:     file,
		DevURL: devURL,
	})
}

func (r *AtlasSchemaReconciler) verifyFirstRun(ctx context.Context, des *managed, devURL string) error {
	tmpdir, err := os.MkdirTemp("", "run-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	ins, err := r.CLI.SchemaInspect(ctx, &atlas.SchemaInspectParams{
		DevURL: devURL,
		URL:    des.url.String(),
		Format: "{{ sql . }}",
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "1.sql"), []byte(ins), 0644); err != nil {
		return err
	}
	desired, clean, err := atlas.TempFile(des.schema, des.ext)
	if err != nil {
		return err
	}
	defer clean()
	dry, err := r.CLI.SchemaApply(ctx, &atlas.SchemaApplyParams{
		DryRun: true,
		URL:    des.url.String(),
		To:     desired,
		DevURL: devURL,
	})
	if err != nil {
		return err
	}
	plan := strings.Join(dry.Changes.Pending, "\n")
	if err := os.WriteFile(filepath.Join(tmpdir, "2.sql"), []byte(plan), 0644); err != nil {
		return err
	}
	lint, err := r.CLI.Lint(ctx, &atlas.LintParams{
		DevURL: devURL,
		DirURL: "file://" + tmpdir,
		Latest: 1,
	})
	if diags := destructive(lint.Files); len(diags) > 0 {
		return destructiveErr{diags: diags}
	}
	if err != nil {
		return err
	}
	return nil
}

func destructive(files []*atlas.FileReport) (checks []sqlcheck.Diagnostic) {
	for _, f := range files {
		for _, r := range f.Reports {
			for _, diag := range r.Diagnostics {
				if strings.HasPrefix(diag.Code, "DS") {
					checks = append(checks, diag)
				}
			}
		}
	}
	return
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

// extractManaged extracts the info about the managed database and its desired state.
func (r *AtlasSchemaReconciler) extractManaged(ctx context.Context, sc *dbv1alpha1.AtlasSchema) (*managed, error) {
	var d managed
	switch sch := sc.Spec.Schema; {
	case sch.HCL != "":
		d.schema = sch.HCL
		d.ext = "hcl"
	case sch.SQL != "":
		d.schema = sch.SQL
		d.ext = "sql"
	default:
		return nil, fmt.Errorf("no desired schema specified")
	}
	u, err := r.url(ctx, sc)
	if err != nil {
		return nil, err
	}
	d.url = u
	d.driver = driver(u.Scheme)
	return &d, nil
}

// hash returns the sha256 hash of the schema.
func (d *managed) hash() string {
	h := sha256.New()
	h.Write([]byte(d.schema))
	return hex.EncodeToString(h.Sum(nil))
}

func setNotReady(sc *dbv1alpha1.AtlasSchema, reason, msg string) {
	meta.SetStatusCondition(
		&sc.Status.Conditions,
		metav1.Condition{
			Type:    schemaReadyCond,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: msg,
		},
	)
}

func setReady(sc *dbv1alpha1.AtlasSchema, des *managed, apply *atlas.SchemaApply) {
	var msg string
	if j, err := json.Marshal(apply); err != nil {
		msg = fmt.Sprintf("Error marshalling apply response: %v", err)
	} else {
		msg = fmt.Sprintf("The schema has been applied successfully. Apply response: %s", j)
	}
	meta.SetStatusCondition(
		&sc.Status.Conditions,
		metav1.Condition{
			Type:    schemaReadyCond,
			Status:  metav1.ConditionTrue,
			Reason:  "Applied",
			Message: msg,
		},
	)
	sc.Status.ObservedHash = des.hash()
	sc.Status.LastApplied = time.Now().Unix()
}

func (d destructiveErr) Error() string {
	var buf strings.Builder
	buf.WriteString("destructive changes detected:\n")
	for _, diag := range d.diags {
		buf.WriteString(diag.Text + "\n")
	}
	return buf.String()
}
