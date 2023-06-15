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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	MigrateReadyCond = "Ready"
)

// AtlasMigrationSpec defines the desired state of AtlasMigration
type AtlasMigrationSpec struct {
	// EnvName sets the environment name used for reporting runs to Atlas Cloud.
	EnvName string `json:"envName,omitempty"`
	// URL of the target database schema.
	URL string `json:"url,omitempty"`
	// URLs may be defined as a secret key reference.
	URLFrom URLFrom `json:"urlFrom,omitempty"`
	// Cloud defines the Atlas Cloud configuration.
	Cloud Cloud `json:"cloud,omitempty"`
	// Dir defines the directory to use for migrations as a configmap key reference.
	Dir Dir `json:"dir"`
	// RevisionsSchema defines the schema that revisions table resides in
	RevisionsSchema string `json:"revisionsSchema,omitempty"`
}

// Cloud defines the Atlas Cloud configuration.
type Cloud struct {
	URL       string    `json:"url,omitempty"`
	TokenFrom TokenFrom `json:"tokenFrom,omitempty"`
	Project   string    `json:"project,omitempty"`
}

// Dir defines the place where migrations are stored.
type Dir struct {
	// ConfigMapRef defines the configmap to use for migrations
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
	// Remote defines the Atlas Cloud directory migration.
	Remote Remote `json:"remote,omitempty"`
}

// Remote defines the Atlas Cloud directory migration.
type Remote struct {
	Name string `json:"name,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// TokenFrom defines a reference to a secret key that contains the Atlas Cloud Token
type TokenFrom struct {
	// SecretKeyRef references to the key of a secret in the same namespace.
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// AtlasMigrationStatus defines the observed state of AtlasMigration
type AtlasMigrationStatus struct {
	// Conditions represent the latest available observations of an object's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastAppliedVersion is the version of the most recent successful versioned migration.
	LastAppliedVersion string `json:"lastAppliedVersion,omitempty"`
	//LastDeploymentURL is the Deployment URL of the most recent successful versioned migration.
	LastDeploymentURL string `json:"lastDeploymentUrl,omitempty"`
	// ObservedHash is the hash of the most recent successful versioned migration.
	ObservedHash string `json:"observed_hash"`
	// LastApplied is the unix timestamp of the most recent successful versioned migration.
	LastApplied int64 `json:"lastApplied"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// AtlasMigration is the Schema for the atlasmigrations API
type AtlasMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtlasMigrationSpec   `json:"spec,omitempty"`
	Status AtlasMigrationStatus `json:"status,omitempty"`
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
	return meta.IsStatusConditionTrue(m.Status.Conditions, MigrateReadyCond)
}

// IsHashModified returns true if the hash is different from the observed hash.
func (m *AtlasMigration) IsHashModified(hash string) bool {
	return hash != m.Status.ObservedHash
}

// SetReady sets the ready condition to true.
func (m *AtlasMigration) SetReady(status AtlasMigrationStatus) {
	m.Status = status
	meta.SetStatusCondition(
		&m.Status.Conditions,
		metav1.Condition{
			Type:   MigrateReadyCond,
			Status: metav1.ConditionTrue,
			Reason: "Applied",
		},
	)
}

// SetNotReady sets the ready condition to false.
func (m *AtlasMigration) SetNotReady(reason, message string) {
	meta.SetStatusCondition(
		&m.Status.Conditions,
		metav1.Condition{
			Type:    MigrateReadyCond,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		},
	)
}

//+kubebuilder:object:root=true

// AtlasMigrationList contains a list of AtlasMigration
type AtlasMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AtlasMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AtlasMigration{}, &AtlasMigrationList{})
}
