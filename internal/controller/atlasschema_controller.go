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
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"ariga.io/atlas-go-sdk/atlasexec"
	"github.com/ariga/atlas-operator/api/v1alpha1"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller/watch"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;update;delete;get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=create;get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=create;delete;get;list;watch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/status,verbs=get;update;patch

type (
	// AtlasSchemaReconciler reconciles a AtlasSchema object
	AtlasSchemaReconciler struct {
		client.Client
		atlasClient      AtlasExecFn
		scheme           *runtime.Scheme
		configMapWatcher *watch.ResourceWatcher
		secretWatcher    *watch.ResourceWatcher
		recorder         record.EventRecorder
		devDB            *devDBReconciler
		// AllowCustomConfig allows the controller to use custom atlas.hcl config.
		allowCustomConfig bool
	}
	// managedData contains information about the managed database and its desired state.
	managedData struct {
		EnvName string
		URL     *url.URL
		DevURL  string
		Schemas []string
		Exclude []string
		Policy  *dbv1alpha1.Policy
		TxMode  dbv1alpha1.TransactionMode
		Desired *url.URL
		Cloud   *Cloud
		Config  *hclwrite.File
		Vars    atlasexec.Vars2
		schema  []byte
	}
)

const sqlLimitSize = 1024

func NewAtlasSchemaReconciler(mgr Manager, prewarmDevDB bool) *AtlasSchemaReconciler {
	rec := mgr.GetEventRecorderFor("atlasschema-controller")
	r := &AtlasSchemaReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         rec,
		devDB:            newDevDB(mgr, rec, prewarmDevDB),
	}
	return r
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = log.FromContext(ctx)
		res = &dbv1alpha1.AtlasSchema{}
	)
	if err = r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if err := r.Status().Update(ctx, res); err != nil {
			log.Error(err, "failed to update resource status")
		}
		// After updating the status, watch the dependent resources
		r.watchRefs(res)
		// Clean up any resources created by the controller after the reconciler is successful.
		if res.IsReady() {
			r.devDB.cleanUp(ctx, res)
		}
	}()
	// When the resource is first created, create the "Ready" condition.
	if len(res.Status.Conditions) == 0 {
		res.SetReconciling("Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	data, err := r.extractData(ctx, res)
	if err != nil {
		return r.resultErr(res, err, "ReadSchema")
	}
	if data.hasTargets() {
		return r.resultErr(res, errors.New("multiple targets are not supported"), "ReadSchema")
	}
	opts := []atlasexec.Option{atlasexec.WithAtlasHCL(data.render)}
	if u := data.Desired; u != nil && u.Scheme == dbv1alpha1.SchemaTypeFile {
		// Write the schema file to the working directory.
		opts = append(opts, func(ce *atlasexec.WorkingDir) error {
			_, err := ce.WriteFile(filepath.Join(u.Host, u.Path), data.schema)
			return err
		})
	}
	if !data.hasDevURL() && data.URL != nil {
		// The user has not specified an URL for dev-db,
		// spin up a dev-db and get the connection string.
		data.DevURL, err = r.devDB.devURL(ctx, res, *data.URL)
		if err != nil {
			return r.resultPending(res, dbv1alpha1.ReasonGettingDevDB, err.Error())
		}
	}
	// Create a working directory for the Atlas CLI
	// The working directory contains the atlas.hcl config.
	wd, err := atlasexec.NewWorkingDir(opts...)
	if err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonCreatingWorkingDir)
	}
	defer wd.Close()
	// This function will be used to edit and re-render the atlas.hcl file in the working directory.
	editAtlasHCL := func(fn func(m *managedData)) error {
		fn(data)
		var buf bytes.Buffer
		if err := data.render(&buf); err != nil {
			return err
		}
		_, err = wd.WriteFile("atlas.hcl", buf.Bytes())
		return err
	}
	cli, err := r.atlasClient(wd.Path(), nil)
	if err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonCreatingAtlasClient)
	}
	// Calculate the hash of the current schema.
	hash, err := cli.SchemaInspect(ctx, &atlasexec.SchemaInspectParams{
		Env:    data.EnvName,
		URL:    data.targetURL(),
		Format: `{{ .Hash }}`,
		Vars:   data.Vars,
	})
	if err != nil {
		return r.resultErr(res, err, "CalculatingHash")
	}
	// We need to update the ready condition immediately before doing
	// any heavy jobs if the hash is different from the last applied.
	// This is to ensure that other tools know we are still applying the changes.
	if res.IsReady() && res.IsHashModified(hash) {
		res.SetReconciling("current schema does not match last applied")
		return ctrl.Result{Requeue: true}, nil
	}
	// ====================================================
	// Starting area to handle the heavy jobs.
	// Below this line is the main logic of the controller.
	// ====================================================
	var whoami *atlasexec.WhoAmI
	switch whoami, err = cli.WhoAmI(ctx, &atlasexec.WhoAmIParams{Vars: data.Vars}); {
	case errors.Is(err, atlasexec.ErrRequireLogin):
		log.Info("the resource is not connected to Atlas Cloud")
		if data.Config != nil {
			return r.resultErr(res, err, dbv1alpha1.ReasonWhoAmI)
		}
	case err != nil:
		res.SetNotReady(dbv1alpha1.ReasonWhoAmI, err.Error())
	default:
		log.Info("the resource is connected to Atlas Cloud", "org", whoami.Org)
	}
	var reports []*atlasexec.SchemaApply
	shouldLint, err := data.shouldLint()
	if err != nil {
		return r.resultErr(res, err, "LintPolicyError")
	}
	switch desiredURL := data.targetURL(); {
	// The resource is connected to Atlas Cloud.
	case whoami != nil:
		err = editAtlasHCL(func(m *managedData) {
			m.enableDestructive(false)
			m.setLintReview(dbv1alpha1.LintReviewError, false)
		})
		if err != nil {
			return r.resultErr(res, err, "ModifyingAtlasHCL")
		}
		params := &atlasexec.SchemaApplyParams{
			Env:    data.EnvName,
			To:     desiredURL,
			TxMode: string(data.TxMode),
			Vars:   data.Vars,
		}
		repo := data.repoURL()
		if repo == nil {
			// No repository is set, apply the changes directly.
			reports, err = cli.SchemaApplySlice(ctx, params)
			break
		}
		createPlan := func() (ctrl.Result, error) {
			// If the desired state is a file, we need to push the schema to the Atlas Cloud.
			// This is to ensure that the schema is in sync with the Atlas Cloud.
			// And the schema is available for the Atlas CLI (on local machine)
			// to modify or approve the changes.
			if data.Desired != nil && data.Desired.Scheme == dbv1alpha1.SchemaTypeFile {
				tag, err := cli.SchemaInspect(ctx, &atlasexec.SchemaInspectParams{
					Env:    data.EnvName,
					URL:    desiredURL,
					Format: `{{ .Hash | base64url }}`,
					Vars:   data.Vars,
				})
				if err != nil {
					return r.resultCLIErr(res, err, "SchemaPush")
				}
				log.Info("schema is a file, pushing the schema to Atlas Cloud")
				state, err := cli.SchemaPush(ctx, &atlasexec.SchemaPushParams{
					Env:  data.EnvName,
					Name: path.Join(repo.Host, repo.Path),
					Tag:  fmt.Sprintf("operator-plan-%.8s", strings.ToLower(tag)),
					URL:  []string{desiredURL},
					Vars: data.Vars,
				})
				if err != nil {
					return r.resultCLIErr(res, err, "SchemaPush")
				}
				desiredURL = state.URL
			}
			log.Info("creating a new schema plan", "desiredURL", desiredURL)
			// Create a new plan for the pending changes.
			plan, err := cli.SchemaPlan(ctx, &atlasexec.SchemaPlanParams{
				Env:     data.EnvName,
				Repo:    repo.String(),
				From:    []string{"env://url"},
				To:      []string{desiredURL},
				Pending: true,
				Vars:    data.Vars,
			})
			switch {
			case err != nil && strings.Contains(err.Error(), "no changes to be made"):
				res.SetReady(dbv1alpha1.AtlasSchemaStatus{
					LastApplied:  time.Now().Unix(),
					ObservedHash: hash,
				}, nil)
				r.recorder.Event(res, corev1.EventTypeNormal, "Applied", "Applied schema")
				return ctrl.Result{}, nil
			case err != nil:
				reason, msg := "SchemaPlan", err.Error()
				r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
				return r.resultCLIErr(res, err, reason)
			default:
				log.Info("created a new schema plan", "plan", plan.File.URL, "desiredURL", desiredURL)
				res.Status.PlanURL = plan.File.URL
				res.Status.PlanLink = plan.File.Link
				return r.resultPending(res, dbv1alpha1.ReasonApprovalPending, "Schema plan is waiting for approval")
			}
		}
		// List the schema plans to check if there are any plans.
		switch plans, err := cli.SchemaPlanList(ctx, &atlasexec.SchemaPlanListParams{
			Env:  data.EnvName,
			Repo: repo.String(),
			From: []string{"env://url"},
			To:   []string{desiredURL},
			Vars: data.Vars,
		}); {
		case err != nil:
			return r.resultCLIErr(res, err, "ListingPlans")
		// There are multiple pending plans. This is an unexpected state.
		case len(plans) > 1:
			planURLs := make([]string, 0, len(plans))
			for _, p := range plans {
				planURLs = append(planURLs, p.URL)
			}
			log.Info("multiple schema plans found", "plans", planURLs)
			return r.resultCLIErr(res, fmt.Errorf("multiple schema plans found: %s", strings.Join(planURLs, ", ")), "ListingPlans")
		// There are no pending plans, but Atlas has been asked to review the changes ALWAYS.
		case len(plans) == 0 && data.Policy.Lint.Review == dbv1alpha1.LintReviewAlways:
			// Create a plan for the pending changes.
			return createPlan()
		// The plan is pending approval, show the plan to the user.
		case len(plans) == 1 && plans[0].Status == "PENDING":
			log.Info("found a pending schema plan, waiting for approval", "plan", plans[0].URL)
			res.Status.PlanURL = plans[0].URL
			res.Status.PlanLink = plans[0].Link
			reason, msg := dbv1alpha1.ReasonApprovalPending, "Schema plan is waiting for approval"
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeNormal, reason, msg)
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil
		// Deploy the changes using the approved plan.
		case len(plans) == 1 && plans[0].Status == "APPROVED":
			log.Info("found an approved schema plan, applying", "plan", plans[0].URL)
			params.PlanURL = plans[0].URL
		}
		// Try to apply the schema changes with lint policies,
		// if the changes are rejected by the review policy, create a plan
		// for the pending changes.
		reports, err = cli.SchemaApplySlice(ctx, params)
		// TODO: Better error handling for rejected changes.
		if err != nil && strings.HasPrefix(err.Error(), "Rejected by review policy") {
			log.Info("schema changes are rejected by the review policy, creating a new schema plan")
			return createPlan()
		}
	// Verify the first run doesn't contain destructive changes.
	case res.Status.LastApplied == 0:
		// For the first run, force the destructive linting policy to true.
		// Then revert it back to the original value after the linting is done.
		if err = editAtlasHCL(func(m *managedData) {
			m.enableDestructive(true)
		}); err != nil {
			return r.resultErr(res, err, "ModifyingAtlasHCL")
		}
		err = r.lint(ctx, wd, data, data.Vars)
		switch d := (*destructiveErr)(nil); {
		case errors.As(err, &d):
			reason, msg := d.FirstRun()
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
			// Don't requeue destructive errors.
			return ctrl.Result{}, nil
		case err != nil:
			return r.resultCLIErr(res, err, "VerifyingFirstRun")
		}
		// Revert the destructive linting policy back to the original value.
		if err = editAtlasHCL(func(m *managedData) {
			m.Policy.Lint.Destructive.Error = false
		}); err != nil {
			return r.resultErr(res, err, "ModifyingAtlasHCL")
		}
		reports, err = cli.SchemaApplySlice(ctx, &atlasexec.SchemaApplyParams{
			Env:         data.EnvName,
			To:          desiredURL,
			TxMode:      string(data.TxMode),
			AutoApprove: true,
			Vars:        data.Vars,
		})
	// Run the linting policy.
	case shouldLint:
		if err = r.lint(ctx, wd, data, nil); err != nil {
			return r.resultCLIErr(res, err, "LintPolicyError")
		}
		reports, err = cli.SchemaApplySlice(ctx, &atlasexec.SchemaApplyParams{
			Env:         data.EnvName,
			To:          desiredURL,
			TxMode:      string(data.TxMode),
			AutoApprove: true,
			Vars:        data.Vars,
		})
	// No linting policy is set.
	default:
		reports, err = cli.SchemaApplySlice(ctx, &atlasexec.SchemaApplyParams{
			Env:         data.EnvName,
			To:          desiredURL,
			TxMode:      string(data.TxMode),
			AutoApprove: true,
			Vars:        data.Vars,
		})
	}
	if err != nil {
		return r.resultCLIErr(res, err, "ApplyingSchema")
	}
	s := dbv1alpha1.AtlasSchemaStatus{
		LastApplied:  time.Now().Unix(),
		ObservedHash: hash,
	}
	if len(reports) != 1 {
		return r.resultErr(res, fmt.Errorf("unexpected number of reports: %d", len(reports)), "ApplyingSchema")
	}
	log.Info("schema changes are applied", "applied", len(reports[0].Changes.Applied))
	// Truncate the applied and pending changes to 1024 bytes.
	reports[0].Changes.Applied = truncateSQL(reports[0].Changes.Applied, sqlLimitSize)
	reports[0].Changes.Pending = truncateSQL(reports[0].Changes.Pending, sqlLimitSize)
	// Set the plan URL if it exists.
	if p := reports[0].Plan; p != nil {
		s.PlanLink = p.File.Link
		s.PlanURL = p.File.URL
	}
	res.SetReady(s, reports[0])
	r.recorder.Event(res, corev1.EventTypeNormal, "Applied", "Applied schema")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasSchema{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasSchema{}).
		Watches(&corev1.ConfigMap{}, r.configMapWatcher).
		Watches(&corev1.Secret{}, r.secretWatcher).
		Complete(r)
}

func (r *AtlasSchemaReconciler) watchRefs(res *dbv1alpha1.AtlasSchema) {
	if c := res.Spec.Schema.ConfigMapKeyRef; c != nil {
		r.configMapWatcher.Watch(
			types.NamespacedName{Name: c.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.Cloud.TokenFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.URLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.Credentials.PasswordFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.DevURLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
}

// SetAtlasClient sets the Atlas client function.
func (r *AtlasSchemaReconciler) SetAtlasClient(fn AtlasExecFn) {
	r.atlasClient = fn
}

// AllowCustomConfig allows the controller to use custom atlas.hcl config.
func (r *AtlasSchemaReconciler) AllowCustomConfig() {
	r.allowCustomConfig = true
}

// extractData extracts the info about the managed database and its desired state.
func (r *AtlasSchemaReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasSchema) (_ *managedData, err error) {
	var (
		s    = res.Spec
		c    = s.Cloud
		data = &managedData{
			EnvName: defaultEnvName,
			DevURL:  s.DevURL,
			Schemas: s.Schemas,
			Exclude: s.Exclude,
			Policy:  s.Policy,
			TxMode:  s.TxMode,
		}
	)
	data.Config, err = s.GetConfig(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	hasConfig := data.Config != nil
	if hasConfig {
		if !r.allowCustomConfig {
			return nil, errors.New("install the operator with \"--set allowCustomConfig=true\" to use custom atlas.hcl config")
		}
		if s.EnvName == "" {
			return nil, errors.New("env name must be set when using custom atlas.hcl config")
		}
	}
	if s := s.EnvName; s != "" {
		data.EnvName = s
	}
	if s := c.TokenFrom.SecretKeyRef; s != nil {
		token, err := getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
		data.Cloud = &Cloud{Token: token}
	}
	if r := c.Repo; r != "" {
		if data.Cloud == nil {
			// The ATLAS_TOKEN token can be provide via the environment variable.
			data.Cloud = &Cloud{}
		}
		data.Cloud.Repo = r
	}
	data.URL, err = s.DatabaseURL(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	if !hasConfig && data.URL == nil {
		return nil, transient(errors.New("no target database defined"))
	}
	data.Desired, data.schema, err = s.Schema.DesiredState(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	if s := s.DevURLFrom.SecretKeyRef; s != nil {
		// SecretKeyRef is set, get the secret value
		// then override the dev url.
		data.DevURL, err = getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
	}
	data.Vars, err = s.GetVars(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	return data, nil
}

func (r *AtlasSchemaReconciler) recordErrEvent(res *dbv1alpha1.AtlasSchema, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

func (r *AtlasSchemaReconciler) resultErr(
	res *dbv1alpha1.AtlasSchema, err error, reason string,
) (ctrl.Result, error) {
	if isConnectionErr(err) {
		err = transient(err)
	}
	res.SetNotReady(reason, err.Error())
	r.recordErrEvent(res, err)
	if res.IsExceedBackoffLimit() {
		r.recorder.Event(res, corev1.EventTypeWarning, "BackoffLimitExceeded", "backoff limit exceeded")
		return result(err, 0)
	}
	return result(err, backoffDelayAt(res.Status.Failed))
}

func (r *AtlasSchemaReconciler) resultCLIErr(
	res *dbv1alpha1.AtlasSchema, err error, reason string,
) (ctrl.Result, error) {
	if !isSQLErr(err) {
		err = transient(err)
	}
	res.SetNotReady(reason, err.Error())
	r.recordErrEvent(res, err)
	if res.IsExceedBackoffLimit() {
		r.recorder.Event(res, corev1.EventTypeWarning, "BackoffLimitExceeded", "Backoff limit exceeded")
		return result(err, 0)
	}
	return result(err, backoffDelayAt(res.Status.Failed))
}

// resultPending returns a pending result.
// The controller will requeue the request after 5 seconds.
func (r *AtlasSchemaReconciler) resultPending(
	res *dbv1alpha1.AtlasSchema, reason, message string,
) (ctrl.Result, error) {
	res.SetNotReady(reason, message)
	r.recorder.Event(res, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{
		RequeueAfter: time.Second * 5,
	}, nil
}

// ShouldLint returns true if the linting policy is set to error.
func (d *managedData) shouldLint() (bool, error) {
	p := d.Policy
	if p == nil || p.Lint == nil || p.Lint.Destructive == nil {
		// Check if the destructive policy is set to error in the custom config.
		// This check won't work if the attribute value has expressions in it.
		if d.Config != nil {
			env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
			if env == nil {
				return false, nil
			}
			lint := searchBlock(env.Body(), hclwrite.NewBlock("lint", nil))
			if lint == nil {
				// search global lint block
				lint = searchBlock(d.Config.Body(), hclwrite.NewBlock("lint", nil))
				if lint == nil {
					return false, nil
				}
			}
			if v := searchBlock(lint.Body(), hclwrite.NewBlock("destructive", nil)); v != nil {
				destructiveErr := v.Body().GetAttribute("error")
				if destructiveErr != nil {
					attrValue := strings.TrimSpace(string(destructiveErr.Expr().BuildTokens(nil).Bytes()))
					attrValue = strings.Trim(attrValue, "\"")
					b, err := strconv.ParseBool(attrValue)
					if err != nil {
						return b, errors.New("cannot determine the value of the destructive.error attribute")
					}
					return b, nil
				}
			}
		}
		return false, nil
	}
	return p.Lint.Destructive.Error, nil
}

// hasDevURL returns true if the environment has a dev URL.
func (d *managedData) hasDevURL() bool {
	if d.DevURL != "" {
		return true
	}
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env != nil {
		dev := env.Body().GetAttribute("dev")
		if dev != nil {
			return true
		}
	}
	return false
}

// hasTargets returns true if the environment has multiple targets/ multi-tenancy.
func (d *managedData) hasTargets() bool {
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return false
	}
	return env.Body().GetAttribute("for_each") != nil
}

// hasLint returns true if the environment has multiple targets/ multi-tenancy.
func (d *managedData) hasLint() bool {
	if d.Policy != nil && d.Policy.HasLint() {
		return true
	}
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return false
	}
	return searchBlock(env.Body(), hclwrite.NewBlock("lint", nil)) != nil
}

// hasLintDestructive returns true if the environment has a lint destructive policy.
func (d *managedData) hasLintDestructive() bool {
	if d.Policy != nil && d.Policy.HasLintDestructive() {
		return true
	}
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return false
	}
	lint := searchBlock(env.Body(), hclwrite.NewBlock("lint", nil))
	if lint == nil {
		// search global lint block
		lint = searchBlock(d.Config.Body(), hclwrite.NewBlock("lint", nil))
		if lint == nil {
			return false
		}
		return false
	}
	destructive := searchBlock(lint.Body(), hclwrite.NewBlock("destructive", nil))
	return destructive != nil && destructive.Body().GetAttribute("error") != nil
}

// hasLintReview returns true if the environment has a lint review policy.
func (d *managedData) hasLintReview() bool {
	if d.Policy != nil && d.Policy.HasLintReview() {
		return true
	}
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return false
	}
	lint := searchBlock(env.Body(), hclwrite.NewBlock("lint", nil))
	if lint == nil {
		// search global lint block
		lint = searchBlock(d.Config.Body(), hclwrite.NewBlock("lint", nil))
		if lint == nil {
			return false
		}
		return false
	}
	return lint.Body().GetAttribute("review") != nil
}

func (d *managedData) repoURL() *url.URL {
	switch {
	// The user has provided the repository name.
	case d.Cloud != nil && d.Cloud.Repo != "":
		return (&url.URL{
			Scheme: v1alpha1.SchemaTypeAtlas,
			Host:   d.Cloud.Repo,
		})
	// Fallback to desired URL if it's Cloud URL.
	case d.Desired != nil && d.Desired.Scheme == dbv1alpha1.SchemaTypeAtlas:
		c := *d.Desired
		c.RawQuery = ""
		return &c
	default:
		return nil
	}
}

// targetURL returns the target URL for the environment.
func (d *managedData) targetURL() string {
	if d.Config != nil {
		env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
		if env != nil && env.Body().GetAttribute("src") != nil {
			return "env://src"
		}
	}
	return "env://schema.src"
}

// render renders the atlas.hcl template.
//
// The template is used by the Atlas CLI to apply the schema.
// It also validates the data before rendering the template.
func (d *managedData) render(w io.Writer) error {
	f := hclwrite.NewFile()
	for _, b := range d.asBlocks() {
		f.Body().AppendBlock(b)
	}
	// Merge config into the atlas.hcl file.
	if d.Config != nil {
		mergeBlocks(f.Body(), d.Config.Body())
	}
	if d.EnvName == "" {
		return errors.New("env name is not set")
	}
	env := searchBlock(f.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return fmt.Errorf("env block %q is not found", d.EnvName)
	}
	b := env.Body()
	if b.GetAttribute("url") == nil {
		return errors.New("database url is not set")
	}
	if b.GetAttribute("dev") == nil {
		return errors.New("dev url is not set")
	}
	schema := searchBlock(b, hclwrite.NewBlock("schema", nil))
	if schema == nil && b.GetAttribute("src") == nil {
		return errors.New("the desired state is not set")
	}
	if _, err := f.WriteTo(w); err != nil {
		return err
	}
	return nil
}

// enableDestructive enables the linting policy for destructive changes.
// If the force is set to true, it will override the existing value.
func (d *managedData) enableDestructive(force bool) {
	override := func() {
		check := &dbv1alpha1.CheckConfig{Error: true}
		destructive := &dbv1alpha1.Lint{Destructive: check}
		if d.Policy == nil {
			d.Policy = &dbv1alpha1.Policy{Lint: destructive}
			return
		}
		if d.Policy.Lint == nil {
			d.Policy.Lint = destructive
			return
		}
		d.Policy.Lint.Destructive = check
	}
	if !d.hasLint() || !d.hasLintDestructive() || force {
		override()
	}
}

// setLintReview sets the lint review policy.
// If the force is set to true, it will override the existing value.
func (d *managedData) setLintReview(v dbv1alpha1.LintReview, force bool) {
	override := func() {
		lint := &dbv1alpha1.Lint{Review: v}
		if d.Policy == nil {
			d.Policy = &dbv1alpha1.Policy{Lint: lint}
			return
		}
		if d.Policy.Lint == nil {
			d.Policy.Lint = lint
			return
		}
		d.Policy.Lint.Review = v
	}
	if !d.hasLint() || !d.hasLintReview() || force {
		override()
	}
}

// asBlocks returns the HCL block for the environment configuration.
func (d *managedData) asBlocks() []*hclwrite.Block {
	var blocks []*hclwrite.Block
	if d.Cloud != nil {
		atlas := hclwrite.NewBlock("atlas", nil)
		cloud := atlas.Body().AppendNewBlock("cloud", nil).Body()
		if d.Cloud.Token != "" {
			cloud.SetAttributeValue("token", cty.StringVal(d.Cloud.Token))
		}
		if d.Cloud.URL != "" {
			cloud.SetAttributeValue("url", cty.StringVal(d.Cloud.URL))
		}
		if d.Cloud.Repo != "" {
			cloud.SetAttributeValue("project", cty.StringVal(d.Cloud.Repo))
		}
		blocks = append(blocks, atlas)
	}
	env := hclwrite.NewBlock("env", []string{d.EnvName})
	blocks = append(blocks, env)
	envBody := env.Body()
	if d.Desired != nil {
		schema := envBody.AppendNewBlock("schema", nil).Body()
		schema.SetAttributeValue("src", cty.StringVal(d.Desired.String()))
	}
	if d.URL != nil {
		envBody.SetAttributeValue("url", cty.StringVal(d.URL.String()))
	}
	if d.DevURL != "" {
		envBody.SetAttributeValue("dev", cty.StringVal(d.DevURL))
	}
	if l := d.Schemas; len(l) > 0 {
		envBody.SetAttributeValue("schemas", listStringVal(l))
	}
	if l := d.Exclude; len(l) > 0 {
		envBody.SetAttributeValue("exclude", listStringVal(l))
	}
	if p := d.Policy; p != nil {
		if d := p.Diff; d != nil {
			envBody.AppendBlock(d.AsBlock())
		}
		if l := p.Lint; l != nil {
			lint := envBody.AppendNewBlock("lint", nil).Body()
			if v := l.Destructive; v != nil {
				b := lint.AppendNewBlock("destructive", nil).Body()
				b.SetAttributeValue("error", cty.BoolVal(v.Error))
			}
			if v := l.Review; v != "" {
				lint.SetAttributeValue("review", cty.StringVal(string(v)))
			}
		}
	}
	if v := d.TxMode; v != "" {
		envBody.SetAttributeValue("tx_mode", cty.StringVal(string(v)))
	}
	return blocks
}

func truncateSQL(s []string, size int) []string {
	total := 0
	for _, v := range s {
		total += len(v)
	}
	if total > size {
		switch idx, len := strings.IndexRune(s[0], '\n'), len(s[0]); {
		case len <= size:
			total -= len
			return []string{s[0], fmt.Sprintf("-- truncated %d bytes...", total)}
		case idx != -1 && idx <= size:
			total -= (idx + 1)
			return []string{fmt.Sprintf("%s\n-- truncated %d bytes...", s[0][:idx], total)}
		default:
			return []string{fmt.Sprintf("-- truncated %d bytes...", total)}
		}
	}
	return s
}
