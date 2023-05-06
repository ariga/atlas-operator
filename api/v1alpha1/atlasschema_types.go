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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AtlasSchemaSpec defines the desired state of AtlasSchema
type AtlasSchemaSpec struct {
	// URL of the target database schema.
	URL string `json:"url,omitempty"`
	// URLs may be defined as a secret key reference.
	URLFrom URLFrom `json:"urlFrom,omitempty"`
	// Desired Schema of the target.
	Schema Schema `json:"schema,omitempty"`
	// Exclude a list of glob patterns used to filter existing resources being taken into account.
	Exclude []string `json:"exclude,omitempty"`
	// Policy defines the policies to apply when managing the schema change lifecycle.
	Policy Policy `json:"policy,omitempty"`
	// The names of the schemas (named databases) on the target database to be managed.
	Schemas []string `json:"schemas,omitempty"`
}

// Policy defines the policies to apply when managing the schema change lifecycle.
type Policy struct {
	Lint Lint `json:"lint,omitempty"`
	Diff Diff `json:"diff,omitempty"`
}

// Lint defines the linting policies to apply before applying the schema.
type Lint struct {
	Destructive CheckConfig `json:"destructive,omitempty"`
}

// Diff defines the diff policies to apply when planning schema changes.
type Diff struct {
	Skip SkipChanges `json:"skip,omitempty"`
}

// SkipChanges represents the skip changes policy.
type SkipChanges struct {
	// +optional
	AddSchema bool `json:"add_schema,omitempty"`
	// +optional
	DropSchema bool `json:"drop_schema,omitempty"`
	// +optional
	ModifySchema bool `json:"modify_schema,omitempty"`
	// +optional
	AddTable bool `json:"add_table,omitempty"`
	// +optional
	DropTable bool `json:"drop_table,omitempty"`
	// +optional
	ModifyTable bool `json:"modify_table,omitempty"`
	// +optional
	AddColumn bool `json:"add_column,omitempty"`
	// +optional
	DropColumn bool `json:"drop_column,omitempty"`
	// +optional
	ModifyColumn bool `json:"modify_column,omitempty"`
	// +optional
	AddIndex bool `json:"add_index,omitempty"`
	// +optional
	DropIndex bool `json:"drop_index,omitempty"`
	// +optional
	ModifyIndex bool `json:"modify_index,omitempty"`
	// +optional
	AddForeignKey bool `json:"add_foreign_key,omitempty"`
	// +optional
	DropForeignKey bool `json:"drop_foreign_key,omitempty"`
	// +optional
	ModifyForeignKey bool `json:"modify_foreign_key,omitempty"`
}

// CheckConfig defines the configuration of a linting check.
type CheckConfig struct {
	Error bool `json:"error,omitempty"`
}

// URLFrom defines a reference to a secret key that contains the Atlas URL of the
// target database schema.
type URLFrom struct {
	// SecretKeyRef references to the key of a secret in the same namespace.
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// Schema defines the desired state of the target database schema in plain SQL or HCL.
type Schema struct {
	SQL string `json:"sql,omitempty"`
	HCL string `json:"hcl,omitempty"`
}

// AtlasSchemaStatus defines the observed state of AtlasSchema
type AtlasSchemaStatus struct {
	// Conditions represent the latest available observations of an object's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedHash is the hash of the most recently applied schema.
	ObservedHash string `json:"observed_hash"`
	// LastApplied is the unix timestamp of the most recent successful schema apply operation.
	LastApplied int64 `json:"last_applied"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// AtlasSchema is the Schema for the atlasschemas API
type AtlasSchema struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtlasSchemaSpec   `json:"spec,omitempty"`
	Status AtlasSchemaStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AtlasSchemaList contains a list of AtlasSchema
type AtlasSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AtlasSchema `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AtlasSchema{}, &AtlasSchemaList{})
}
