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
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestTemplateSanity(t *testing.T) {
	var b bytes.Buffer
	v := &devDB{
		NamespacedName: types.NamespacedName{
			Name:      "test",
			Namespace: "default",
		},
	}
	for _, tt := range []string{"mysql", "postgres", "sqlserver"} {
		t.Run(tt, func(t *testing.T) {
			v.Driver = tt
			err := tmpl.ExecuteTemplate(&b, "devdb.tmpl", v)
			require.NoError(t, err)
			var d appsv1.Deployment
			err = yaml.NewYAMLToJSONDecoder(&b).Decode(&d)
			require.NoError(t, err)
			b.Reset()
		})
	}
}
