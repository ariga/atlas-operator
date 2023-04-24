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
