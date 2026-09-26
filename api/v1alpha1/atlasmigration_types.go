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

package v1alpha1

import (
	"cmp"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type (
	//+kubebuilder:object:root=true
	//
	// AtlasMigrationList contains a list of AtlasMigration
	AtlasMigrationList struct {
		metav1.TypeMeta `json:",inline"`
		metav1.ListMeta `json:"metadata,omitempty"`

		Items []AtlasMigration `json:"items"`
	}
	//+kubebuilder:object:root=true
	//+kubebuilder:subresource:status
	//
	// AtlasMigration is the Schema for the atlasmigrations API
	// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
	// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
	AtlasMigration struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`

		Spec AtlasMigrationSpec `json:"spec,omitempty"`
		//+kubebuilder:default={"observedGeneration":-1}
		Status AtlasMigrationStatus `json:"status,omitempty"`
	}
	// AtlasMigrationStatus defines the observed state of AtlasMigration
	AtlasMigrationStatus struct {
		// ObservedGeneration is the generation last processed by the controller.
		// +optional
		ObservedGeneration int64 `json:"observedGeneration,omitempty"`
		// Conditions represent the latest available observations of an object's state.
		// +optional
		Conditions []metav1.Condition `json:"conditions,omitempty"`
		// LastAppliedVersion is the version of the most recent successful versioned migration.
		// +optional
		LastAppliedVersion string `json:"lastAppliedVersion,omitempty"`
		// LastDeploymentURL is the Deployment URL of the most recent successful versioned migration.
		// +optional
		LastDeploymentURL string `json:"lastDeploymentUrl,omitempty"`
		// ApprovalURL is the URL to approve the migration.
		// +optional
		ApprovalURL string `json:"approvalUrl,omitempty"`
		// ObservedHash is the hash of the most recent successful versioned migration.
		// +optional
		ObservedHash string `json:"observed_hash"`
		// LastApplied is the unix timestamp of the most recent successful versioned migration.
		// +optional
		LastApplied int64 `json:"lastApplied"`
		// Failed is the number of times the migration has failed.
		// +optional
		// +kubebuilder:default=0
		Failed int `json:"failed"`
	}
	// AtlasMigrationSpec defines the desired state of AtlasMigration
	AtlasMigrationSpec struct {
		TargetSpec        `json:",inline"`
		ProjectConfigSpec `json:",inline"`
		// EnvName sets the environment name used for reporting runs to Atlas Cloud.
		EnvName string `json:"envName,omitempty"`
		// Cloud defines the Atlas Cloud configuration.
		Cloud CloudV0 `json:"cloud,omitempty"`
		// Dir defines the directory to use for migrations as a configmap key reference.
		Dir Dir `json:"dir"`
		// DevURL is the URL of the database to use for normalization and calculations.
		// If not specified, the operator will spin up a temporary database container to use for these operations.
		// +optional
		DevURL string `json:"devURL"`
		// DevURLFrom is a reference to a secret containing the URL of the database to use for normalization and calculations.
		// +optional
		DevURLFrom Secret `json:"devURLFrom,omitempty"`
		// RevisionsSchema defines the schema that revisions table resides in
		RevisionsSchema string `json:"revisionsSchema,omitempty"`
		// BaselineVersion defines the baseline version of the database on the first migration.
		Baseline string `json:"baseline,omitempty"`
		// ExecOrder controls how Atlas computes and executes pending migration files to the database.
		// +kubebuilder:default=linear
		ExecOrder MigrateExecOrder `json:"execOrder,omitempty"`
		// ProtectedFlows defines the protected flows of a deployment.
		ProtectedFlows *ProtectFlows `json:"protectedFlows,omitempty"`
		// Policy defines the policies to apply when migrating the database.
		// +optional
		Policy *MigrationPolicy `json:"policy,omitempty"`
		// BackoffLimit is the number of retries on error.
		// +kubebuilder:default=20
		BackoffLimit int `json:"backoffLimit,omitempty"`
	}
	CloudV0 struct {
		URL       string    `json:"url,omitempty"`
		TokenFrom TokenFrom `json:"tokenFrom,omitempty"`
		Project   string    `json:"project,omitempty"`
	}
	// TokenFrom defines a reference to a secret key that contains the Atlas Cloud Token
	TokenFrom struct {
		// SecretKeyRef references to the key of a secret in the same namespace.
		SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	}
	// Dir defines the place where migrations are stored.
	Dir struct {
		// ConfigMapRef defines the configmap to use for migrations
		ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
		// Remote defines the Atlas Cloud migration directory.
		Remote Remote `json:"remote,omitempty"`
		// Local defines the local migration directory.
		Local map[string]string `json:"local,omitempty"`
	}
	// Remote defines the Atlas Cloud directory migration.
	Remote struct {
		Name string `json:"name,omitempty"`
		Tag  string `json:"tag,omitempty"`
	}
	// ProtectedFlows defines the protected flows of a deployment.
	ProtectFlows struct {
		MigrateDown *DeploymentFlow `json:"migrateDown,omitempty"`
	}
	// DeploymentFlow defines the flow of a deployment.
	DeploymentFlow struct {
		// Allow allows the flow to be executed.
		// +kubebuilder:default=false
		Allow bool `json:"allow,omitempty"`
		// AutoApprove allows the flow to be automatically approved.
		// +kubebuilder:default=false
		AutoApprove bool `json:"autoApprove,omitempty"`
	}
	// MigrationPolicy defines the policies to apply when migrating the database.
	MigrationPolicy struct {
		// Drift enables the pre-apply drift check
		// Before applying pending migrations, Atlas compares the database with the
		// state the Atlas Registry holds for the current version. The migration
		// directory must be on the registry: set spec.dir.remote, or migration.repo.name
		// in spec.config. See https://atlasgo.io/versioned/drift-detection.
		// +optional
		Drift *DriftPolicy `json:"drift,omitempty"`
	}
	// DriftPolicy configures the pre-apply drift check. It adds a
	// check "migrate_apply" { drift { ... } } block to the generated atlas.hcl.
	DriftPolicy struct {
		// OnError controls what happens when drift is found (default: FAIL).
		// FAIL stops the migration and reports the drift on the Ready condition.
		// CONTINUE applies it anyway and records the drift only in the Atlas
		// +optional
		OnError DriftOnError `json:"onError,omitempty"`
		// Exclude lists glob patterns of database objects to ignore, e.g. "public.audit_*"
		// or "*[type=extension]". It replaces the env-level exclude list.
		// The revisions table is always excluded.
		// +optional
		Exclude []string `json:"exclude,omitempty"`
	}
)

// ExecOrder controls how Atlas computes and executes pending migration files to the database.
// +kubebuilder:validation:Enum=linear;linear-skip;non-linear
type MigrateExecOrder string

// DriftOnError controls what the drift check does when it finds drift.
// +kubebuilder:validation:Enum=FAIL;CONTINUE
type DriftOnError string

// DriftOnError values.
const (
	// DriftOnErrorFail stops the migration when drift is found.
	DriftOnErrorFail DriftOnError = "FAIL"
	// DriftOnErrorContinue applies the migration anyway and records the
	// drift in the Atlas Registry deployment log.
	DriftOnErrorContinue DriftOnError = "CONTINUE"
)

const (
	readyCond       = "Ready"
	reconcilingCond = "Reconciling"
	stalledCond     = "Stalled"
	driftedCond     = "Drifted"
)

func init() {
	SchemeBuilder.Register(&AtlasMigration{}, &AtlasMigrationList{})
}

// NamespacedName returns the namespaced name of the object.
func (m *AtlasMigration) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      m.Name,
		Namespace: m.Namespace,
	}
}

// IsReady returns true if the ready condition is true.
func (m *AtlasMigration) IsReady() bool {
	return meta.IsStatusConditionTrue(m.Status.Conditions, readyCond)
}

// IsReconciling returns true if the reconciling condition is true, i.e. an
// apply is in flight.
func (m *AtlasMigration) IsReconciling() bool {
	return meta.IsStatusConditionTrue(m.Status.Conditions, reconcilingCond)
}

// IsHashModified returns true if the hash is different from the observed hash.
func (m *AtlasMigration) IsHashModified(hash string) bool {
	return hash != m.Status.ObservedHash
}

// SetReconciling sets the ready condition to false with the reason "Reconciling".
func (m *AtlasMigration) SetReconciling(message string) {
	m.Status.ObservedGeneration = m.Generation
	m.setCondition(metav1.Condition{
		Type:    readyCond,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonReconciling,
		Message: message,
	})
	m.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonReconciling,
		Message: message,
	})
	m.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonReconciling,
	})
	m.ResetFailed()
}

// SetReady sets the ready condition to true.
func (m *AtlasMigration) SetReady(status AtlasMigrationStatus) {
	status.ObservedGeneration = m.Generation
	m.Status = status
	m.setCondition(metav1.Condition{
		Type:   readyCond,
		Status: metav1.ConditionTrue,
		Reason: ReasonApplied,
	})
	m.setCondition(metav1.Condition{
		Type:   reconcilingCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonApplied,
	})
	m.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonApplied,
	})
	m.ResetFailed()
}

// SetNotReady sets the ready condition to false.
func (m *AtlasMigration) SetNotReady(reason, message string) {
	m.Status.ObservedGeneration = m.Generation
	m.setCondition(metav1.Condition{
		Type:    readyCond,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	if isFailedReason(reason) {
		m.setCondition(metav1.Condition{
			Type:    reconcilingCond,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		m.setCondition(metav1.Condition{
			Type:    stalledCond,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
		m.IncrementFailed()
		return
	}
	m.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	m.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: reason,
	})
}

// IncrementFailed increments the failed count.
func (m *AtlasMigration) IncrementFailed() {
	m.Status.Failed++
}

// ResetFailed resets the failed count.
func (m *AtlasMigration) ResetFailed() {
	m.Status.Failed = 0
}

// IsExceedBackoffLimit returns true if the failed count exceeds the backoff limit.
func (m *AtlasMigration) IsExceedBackoffLimit() bool {
	if m.Spec.BackoffLimit == 0 {
		return false
	}
	return m.Status.Failed > m.Spec.BackoffLimit
}

func (m *AtlasMigration) setCondition(cond metav1.Condition) {
	if cond.ObservedGeneration <= 0 {
		cond.ObservedGeneration = m.Generation
	}
	meta.SetStatusCondition(&m.Status.Conditions, cond)
}

// HasDrift reports whether the policy enables the pre-apply drift check.
func (p *MigrationPolicy) HasDrift() bool {
	return p != nil && p.Drift != nil
}

// AsBlock returns the check "migrate_apply" block for this policy.
func (d *DriftPolicy) AsBlock() *hclwrite.Block {
	blk := hclwrite.NewBlock("check", []string{"migrate_apply"})
	drift := blk.Body().AppendNewBlock("drift", nil).Body()
	drift.SetAttributeTraversal("on_error", hcl.Traversal{
		hcl.TraverseRoot{Name: string(cmp.Or(d.OnError, DriftOnErrorFail))},
	})
	if len(d.Exclude) > 0 {
		vals := make([]cty.Value, len(d.Exclude))
		for i, e := range d.Exclude {
			vals[i] = cty.StringVal(e)
		}
		drift.SetAttributeValue("exclude", cty.ListVal(vals))
	}
	return blk
}
