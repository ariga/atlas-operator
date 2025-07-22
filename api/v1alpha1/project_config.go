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
	"context"
	"fmt"

	"ariga.io/atlas-go-sdk/atlasexec"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	// ProjectConfigSpec defines the project configuration.
	ProjectConfigSpec struct {
		// Config defines the project configuration.
		// Should be a valid YAML string.
		Config string `json:"config,omitempty"`
		// ConfigFrom defines the reference to the secret key that contains the project configuration.
		ConfigFrom Secret `json:"configFrom,omitempty"`
		// EnvName defines the environment name that defined in the project configuration.
		// If not defined, the default environment "k8s" will be used.
		EnvName string `json:"envName,omitempty"`
		// Vars defines the input variables for the project configuration.
		Vars []Variable `json:"vars,omitempty"`
		// CustomDevDB defines the custom dev database pod spec to use for normalization and calculations.
		// +optional
		CustomDevDB *CustomDevDB `json:"customDevDB,omitempty"`
	}
	// Variables defines the reference of secret/configmap to the input variables for the project configuration.
	Variable struct {
		Key       string    `json:"key,omitempty"`
		Value     string    `json:"value,omitempty"`
		ValueFrom ValueFrom `json:"valueFrom,omitempty"`
	}
	// ValueFrom defines the reference to the secret key that contains the value.
	ValueFrom struct {
		// SecretKeyRef defines the secret key reference to use for the value.
		SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
		// ConfigMapKeyRef defines the configmap key reference to use for the value.
		ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
	}
	CustomDevDB struct {
		Spec corev1.PodSpec `json:"spec,omitempty"`
	}
)

// GetConfig returns the project configuration.
// The configuration is resolved from the secret reference.
func (s ProjectConfigSpec) GetConfig(ctx context.Context, r client.Reader, ns string) (*hclwrite.File, error) {
	rawConfig := s.Config
	if s.ConfigFrom.SecretKeyRef != nil {
		cfgFromSecret, err := getSecretValue(ctx, r, ns, s.ConfigFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		rawConfig = cfgFromSecret
	}
	if rawConfig == "" {
		return nil, nil
	}
	config, diags := hclwrite.ParseConfig([]byte(rawConfig), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse project configuration: %v", diags)
	}
	return config, nil
}

// GetVars returns the input variables for the project configuration.
// The variables are resolved from the secret or configmap reference.
func (s ProjectConfigSpec) GetVars(ctx context.Context, r client.Reader, ns string) (atlasexec.Vars2, error) {
	vars := atlasexec.Vars2{}
	for _, variable := range s.Vars {
		var (
			value string
			err   error
		)
		value = variable.Value
		if variable.ValueFrom.SecretKeyRef != nil {
			if value, err = getSecretValue(ctx, r, ns, variable.ValueFrom.SecretKeyRef); err != nil {
				return nil, err
			}
		}
		if variable.ValueFrom.ConfigMapKeyRef != nil {
			if value, err = getConfigMapValue(ctx, r, ns, variable.ValueFrom.ConfigMapKeyRef); err != nil {
				return nil, err
			}
		}
		// Resolve variables with the same key by grouping them into a slice.
		// It's necessary when generating Atlas command for list(string) input type.
		if existingValue, exists := vars[variable.Key]; exists {
			if _, ok := existingValue.([]string); ok {
				vars[variable.Key] = append(existingValue.([]string), value)
			} else if _, ok := existingValue.(string); ok {
				vars[variable.Key] = []string{existingValue.(string), value}
			} else {
				return nil, fmt.Errorf("invalid variable type for %q", variable.Key)
			}
		}
		vars[variable.Key] = value
	}
	return vars, nil
}
