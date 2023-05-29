/*

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
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/atlas"
)

// CLI is the interface used to interact with Atlas CLI
type MigrateCLI interface {
	Apply(ctx context.Context, data *atlas.ApplyParams) (*atlas.ApplyReport, error)
	Status(ctx context.Context, data *atlas.StatusParams) (*atlas.StatusReport, error)
}

// AtlasMigrationReconciler reconciles a AtlasMigration object
type AtlasMigrationReconciler struct {
	client.Client
	CLI    MigrateCLI
	Scheme *runtime.Scheme
}

// atlasMigrationData is the data used to render the HCL template
// that will be used for Atlas CLI
type (
	atlasMigrationData struct {
		URL       string
		Migration *migration
		Cloud     *cloud
	}

	migration struct {
		Dir string
	}

	cloud struct {
		URL       string
		Token     string
		Project   string
		RemoteDir *remoteDir
	}

	remoteDir struct {
		Name string
		Tag  string
	}
)

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var (
		am  dbv1alpha1.AtlasMigration
		err error
	)

	// Get AtlasMigration resource
	if err = r.Get(ctx, req.NamespacedName, &am); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// At the end of reconcile, update the status of the resource base on the error
	defer func() {
		clientErr := r.updateResourceStatus(ctx, am, err)
		if clientErr != nil {
			log.Error(clientErr, "failed to update resource status")
		}
	}()

	// Only the 'latest' version is supported
	if am.Spec.Version != "latest" {
		err = fmt.Errorf("unsupported version: %s", am.Spec.Version)
		return ctrl.Result{}, nil
	}

	// Reconcile given resource
	am.Status, err = r.reconcile(ctx, am)
	if err != nil {
		return result(err)
	}

	return ctrl.Result{}, nil
}

// Reconcile the given AtlasMigration resource.
func (r *AtlasMigrationReconciler) reconcile(
	ctx context.Context,
	am dbv1alpha1.AtlasMigration) (dbv1alpha1.AtlasMigrationStatus, error) {

	// Extract migration data from the given resource
	migrationData, cleanUp, err := r.extractMigrationData(ctx, am)
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	defer cleanUp()

	// Check if there are any pending migration files
	status, err := r.CLI.Status(ctx, &atlas.StatusParams{URL: migrationData.URL, DirURL: migrationData.Migration.Dir})
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	if len(status.Pending) == 0 {
		return am.Status, nil
	}

	// Create atlas.hcl from template data
	atlasHCL, cleanUp, err := migrationData.render()
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	defer cleanUp()

	// Execute Atlas CLI migrate command
	report, err := r.CLI.Apply(ctx, &atlas.ApplyParams{ConfigURL: atlasHCL})
	if err != nil {
		if !isSQLErr(err) {
			err = transient(err)
		}
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}

	return dbv1alpha1.AtlasMigrationStatus{
		LastApplied:        report.End.Unix(),
		LastAppliedVersion: report.Target,
	}, nil
}

// Extract migration data from the given resource
func (r *AtlasMigrationReconciler) extractMigrationData(
	ctx context.Context,
	am dbv1alpha1.AtlasMigration) (atlasMigrationData, func() error, error) {
	var (
		tmplData atlasMigrationData
		err      error
	)

	// Get database connection string
	tmplData.URL = am.Spec.URL
	if tmplData.URL == "" {
		tmplData.URL, err = r.getSecretValue(ctx, am.Namespace, *am.Spec.URLFrom.SecretKeyRef)
		if err != nil {
			return tmplData, nil, err
		}
	}

	// Get temporary directory
	cleanUpDir := func() error { return nil }
	if am.Spec.Dir.ConfigMapRef != "" {
		tmplData.Migration = &migration{}
		tmplData.Migration.Dir, cleanUpDir, err = r.createTmpDir(ctx, am.Namespace, am.Spec.Dir)
		if err != nil {
			return tmplData, nil, err
		}
	}

	// Get Atlas Cloud Token from secret
	if am.Spec.Cloud.TokenFrom.SecretKeyRef != nil {
		tmplData.Cloud = &cloud{
			URL:     am.Spec.Cloud.URL,
			Project: am.Spec.Cloud.Project,
			RemoteDir: &remoteDir{
				Name: am.Spec.Dir.Remote.Name,
				Tag:  am.Spec.Dir.Remote.Tag,
			},
		}

		tmplData.Cloud.Token, err = r.getSecretValue(ctx, am.Namespace, *am.Spec.Cloud.TokenFrom.SecretKeyRef)
		if err != nil {
			return tmplData, nil, err
		}
	}

	return tmplData, cleanUpDir, nil
}

// Get the value of the given secret key selector.
func (r *AtlasMigrationReconciler) getSecretValue(
	ctx context.Context,
	ns string,
	selector corev1.SecretKeySelector) (string, error) {

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: selector.Name}, secret); err != nil {
		return "", transient(err)
	}

	us := string(secret.Data[selector.Key])
	return us, nil
}

// createTmpDir creates a temporary directory and returns its url.
func (r *AtlasMigrationReconciler) createTmpDir(
	ctx context.Context,
	ns string,
	dir dbv1alpha1.Dir) (string, func() error, error) {

	// Get configmap
	configMap := corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      dir.ConfigMapRef,
	}, &configMap); err != nil {
		return "", nil, transient(err)
	}

	// Create temporary directory and remove it at the end of the function
	tmpDir, err := ioutil.TempDir("", "migrations")
	if err != nil {
		return "", nil, err
	}

	// Foreach configmap to build temporary directory
	// key is the name of the file and value is the content of the file
	for key, value := range configMap.Data {
		filePath := filepath.Join(tmpDir, key)
		err := ioutil.WriteFile(filePath, []byte(value), 0644)
		if err != nil {
			// Remove the temporary directory if there is an error
			os.RemoveAll(tmpDir)
			return "", nil, err
		}
	}

	return fmt.Sprintf("file://%s", tmpDir), func() error {
		return os.RemoveAll(tmpDir)
	}, nil
}

// Update the status of the given AtlasMigration resource.
// If err is not nil, the status will be set to false and the reason will be set to the error message.
func (r *AtlasMigrationReconciler) updateResourceStatus(
	ctx context.Context,
	am dbv1alpha1.AtlasMigration, err error) error {
	conditionStatus := metav1.ConditionTrue
	conditionMessage := ""
	// Be careful when editing this default value, it required when updating the status of the resource
	conditionReason := "Applied"

	if err != nil {
		conditionStatus = metav1.ConditionFalse
		conditionReason = "Reconciling"
		conditionMessage = strings.TrimSpace(err.Error())
	}

	meta.SetStatusCondition(
		&am.Status.Conditions,
		metav1.Condition{
			Type:    "Ready",
			Status:  conditionStatus,
			Reason:  conditionReason,
			Message: conditionMessage,
		},
	)

	if err := r.Status().Update(ctx, &am); err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasMigration{}).
		Complete(r)
}

// Render atlas.hcl file from the given data
func (amd atlasMigrationData) render() (string, func() error, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "atlas_migration.tmpl", amd); err != nil {
		return "", nil, err
	}

	return atlas.TempFile(buf.String(), "hcl")
}
