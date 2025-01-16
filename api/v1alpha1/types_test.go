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

package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ariga/atlas-operator/api/v1alpha1"
)

// This test ensure the priority of methods
// to get the database URL:
// URLFrom > URL > Credentials.PasswordFrom > Credentials > error
func TestTargetSpec_DatabaseURL(t *testing.T) {
	var (
		ctx    = context.Background()
		target = v1alpha1.TargetSpec{}
		client = testclient.NewClientBuilder().
			WithObjects(&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"user":     []byte("nobody"),
					"host":     []byte("default"),
					"url":      []byte("mysql://root:root@localhost:3306/secret"),
					"password": []byte("123456"),
				},
			}).
			Build()
		equal = func(a string) {
			u, err := target.DatabaseURL(ctx, client, "default")
			require.NoError(t, err)
			require.Equal(t, a, u.String())
		}
	)

	// Should return the URL from the credentials
	target.Credentials = v1alpha1.Credentials{
		Scheme:   "mysql",
		Host:     "localhost",
		Port:     3306,
		Database: "local",
		User:     "nobody",
		Password: "secret",
	}
	equal("mysql://nobody:secret@localhost:3306/local")

	// Should return the User from the secret
	target.Credentials.UserFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "user",
	}
	equal("mysql://nobody:secret@localhost:3306/local")

	// Should return the Host from the secret
	target.Credentials.HostFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "host",
	}
	equal("mysql://nobody:secret@default:3306/local")

	// Should return the URL from the credentials and the password from the secret
	target.Credentials.PasswordFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "password",
	}
	equal("mysql://nobody:123456@default:3306/local")

	// Should return the same URL if explicitly defined
	target.URL = "mysql://root:root@localhost:3306/test"
	equal(target.URL)

	// Should return the URL from the secret
	target.URLFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "url",
	}
	equal("mysql://root:root@localhost:3306/secret")
}

func TestSchema_Content(t *testing.T) {
	var (
		ctx    = context.Background()
		sch    = v1alpha1.Schema{}
		client = testclient.NewClientBuilder().
			WithObjects(&v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Data: map[string]string{
					"schema.bug": `boo`,
					"schema.hcl": `foo`,
					"schema.sql": `bar`,
				},
			}).
			Build()
	)
	sch.SQL = "bar"
	u, data, err := sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.sql", u.String())
	require.Equal(t, []byte("bar"), data)

	sch.HCL = "foo"
	u, data, err = sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.hcl", u.String())
	require.Equal(t, []byte("foo"), data)

	// Should return the content from the configmap
	sch.ConfigMapKeyRef = &v1.ConfigMapKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "schema.sql",
	}
	u, data, err = sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.sql", u.String())
	require.Equal(t, []byte("bar"), data)

	sch.ConfigMapKeyRef.Key = "schema.bug"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps key "schema.bug" must be ending with .sql or .hcl, received ".bug"`)

	sch.ConfigMapKeyRef.Key = "schema.foo"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps default/test does not contain key "schema.foo"`)

	sch.ConfigMapKeyRef.Name = "foo"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps "foo" not found`)
}

func TestCredentials_URL(t *testing.T) {
	for _, tt := range []struct {
		c   v1alpha1.Credentials
		exp string
	}{
		{
			c: v1alpha1.Credentials{
				Scheme:   "postgres",
				User:     "user",
				Password: "pass",
				Host:     "host",
				Port:     5432,
				Database: "db",
				Parameters: map[string]string{
					"sslmode": "disable",
				},
			},
			exp: "postgres://user:pass@host:5432/db?sslmode=disable",
		},
		{
			c: v1alpha1.Credentials{
				Scheme: "sqlite",
				Host:   "file",
				Parameters: map[string]string{
					"mode": "memory",
				},
			},
			exp: "sqlite://file?mode=memory",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "host",
				Database: "db",
			},
			exp: "mysql://user:pass@host/db",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "",
				Port:     3306,
				Database: "db",
			},
			exp: "mysql://user:pass@:3306/db",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "sqlserver",
				User:     "sa",
				Password: "P@ssw0rd0995",
				Host:     "",
				Port:     1433,
				Database: "master",
			},
			exp: "sqlserver://sa:P%40ssw0rd0995@:1433?database=master",
		},
	} {
		t.Run(tt.exp, func(t *testing.T) {
			require.Equal(t, tt.exp, tt.c.URL().String())
		})
	}
}
