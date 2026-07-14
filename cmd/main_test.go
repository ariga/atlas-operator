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

package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
)

func TestCacheOptions(t *testing.T) {
	t.Run("no selector and no namespaces leaves the cache unrestricted", func(t *testing.T) {
		opts, err := cacheOptions("", nil)
		require.NoError(t, err)
		require.Nil(t, opts.ByObject)
		require.Nil(t, opts.DefaultNamespaces)
	})
	t.Run("valid selector restricts the managed resource types", func(t *testing.T) {
		opts, err := cacheOptions("app=foo,env in (prod,staging)", nil)
		require.NoError(t, err)
		require.Nil(t, opts.DefaultNamespaces)
		// Only AtlasSchema and AtlasMigration are filtered; Secrets and
		// ConfigMaps stay unrestricted.
		require.Len(t, opts.ByObject, 2)
		byType := map[string]labels.Selector{}
		for obj, bo := range opts.ByObject {
			require.NotNil(t, bo.Label)
			byType[fmt.Sprintf("%T", obj)] = bo.Label
		}
		for _, typ := range []string{"*v1alpha1.AtlasSchema", "*v1alpha1.AtlasMigration"} {
			sel, ok := byType[typ]
			require.Truef(t, ok, "expected a label selector for %s", typ)
			require.True(t, sel.Matches(labels.Set{"app": "foo", "env": "prod"}))
			require.False(t, sel.Matches(labels.Set{"app": "foo", "env": "dev"}))
			require.False(t, sel.Matches(labels.Set{"app": "bar", "env": "prod"}))
		}
	})
	t.Run("namespaces restrict the watched namespaces", func(t *testing.T) {
		opts, err := cacheOptions("", []string{"team-a", "team-b"})
		require.NoError(t, err)
		require.Nil(t, opts.ByObject)
		require.Len(t, opts.DefaultNamespaces, 2)
		require.Contains(t, opts.DefaultNamespaces, "team-a")
		require.Contains(t, opts.DefaultNamespaces, "team-b")
	})
	t.Run("namespaces and selector can be combined", func(t *testing.T) {
		opts, err := cacheOptions("team=a", []string{"team-a"})
		require.NoError(t, err)
		require.Len(t, opts.DefaultNamespaces, 1)
		require.Contains(t, opts.DefaultNamespaces, "team-a")
		require.Len(t, opts.ByObject, 2)
	})
	t.Run("invalid selector returns an error", func(t *testing.T) {
		_, err := cacheOptions("app=foo,,,", nil)
		require.Error(t, err)
	})
}

func TestParseNamespaces(t *testing.T) {
	require.Nil(t, parseNamespaces(""))
	require.Nil(t, parseNamespaces("  , ,"))
	require.Equal(t, []string{"a"}, parseNamespaces("a"))
	require.Equal(t, []string{"a", "b"}, parseNamespaces("a,b"))
	require.Equal(t, []string{"a", "b"}, parseNamespaces(" a , b , "))
}
