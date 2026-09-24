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
	"maps"
	"testing"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDeploymentDevDB_Metadata(t *testing.T) {
	metadata := &dbv1alpha1.DevDBMetadata{
		Labels:      map[string]string{"team": "platform", labelEngine: "override", labelInstance: "override", "app.kubernetes.io/name": "override", "app.kubernetes.io/part-of": "override", "app.kubernetes.io/created-by": "override"},
		Annotations: map[string]string{"example.com/monitor": "enabled", annoConnTmpl: "override"},
	}
	labels, annotations := maps.Clone(metadata.Labels), maps.Clone(metadata.Annotations)
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "custom", Image: "postgres:18"}}}
	deploy := deploymentDevDB(types.NamespacedName{Name: "example-atlas-dev-db", Namespace: "default"}, dbv1alpha1.DriverPostgres, spec, "postgres://localhost/dev", metadata)
	require.Equal(t, map[string]string{
		labelEngine: "postgres", labelInstance: "example-atlas-dev-db",
		"app.kubernetes.io/name": "atlas-dev-db", "app.kubernetes.io/part-of": "atlas-operator", "app.kubernetes.io/created-by": "controller-manager",
	}, deploy.Spec.Selector.MatchLabels)
	expected := maps.Clone(deploy.Spec.Selector.MatchLabels)
	expected["team"] = "platform"
	require.Equal(t, expected, deploy.Spec.Template.Labels)
	require.Equal(t, map[string]string{"example.com/monitor": "enabled", annoConnTmpl: "postgres://localhost/dev"}, deploy.Spec.Template.Annotations)
	require.Equal(t, spec, deploy.Spec.Template.Spec)
	require.Equal(t, labels, metadata.Labels)
	require.Equal(t, annotations, metadata.Annotations)
}
