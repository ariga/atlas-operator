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
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	// TargetSpec defines the target database to manage.
	TargetSpec struct {
		// URL of the target database schema.
		URL string `json:"url,omitempty"`
		// URLs may be defined as a secret key reference.
		URLFrom Secret `json:"urlFrom,omitempty"`
		// Credentials defines the credentials to use when connecting to the database.
		// Used instead of URL or URLFrom.
		Credentials Credentials `json:"credentials,omitempty"`
	}
	// Credentials defines the credentials to use when connecting to the database.
	Credentials struct {
		Scheme       string            `json:"scheme,omitempty"`
		User         string            `json:"user,omitempty"`
		UserFrom     Secret            `json:"userFrom,omitempty"`
		Password     string            `json:"password,omitempty"`
		PasswordFrom Secret            `json:"passwordFrom,omitempty"`
		Host         string            `json:"host,omitempty"`
		HostFrom     Secret            `json:"hostFrom,omitempty"`
		Port         int               `json:"port,omitempty"`
		Database     string            `json:"database,omitempty"`
		Parameters   map[string]string `json:"parameters,omitempty"`
	}
	// Secret defines a secret key reference.
	Secret struct {
		// SecretKeyRef defines the secret key reference to use for the user.
		SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	}
	// Cloud defines the Atlas Cloud configuration.
	Cloud struct {
		// TokenFrom defines the reference to the secret key that contains the Atlas Cloud Token.
		TokenFrom TokenFrom `json:"tokenFrom,omitempty"`
	}
)

// DatabaseURL returns the database url.
func (s TargetSpec) DatabaseURL(ctx context.Context, r client.Reader, ns string) (*url.URL, error) {
	if s.URLFrom.SecretKeyRef != nil {
		val, err := getSecrectValue(ctx, r, ns, s.URLFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		return url.Parse(val)
	}
	if s.URL != "" {
		return url.Parse(s.URL)
	}
	if s.Credentials.UserFrom.SecretKeyRef != nil {
		val, err := getSecrectValue(ctx, r, ns, s.Credentials.UserFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		s.Credentials.User = val
	}
	if s.Credentials.PasswordFrom.SecretKeyRef != nil {
		val, err := getSecrectValue(ctx, r, ns, s.Credentials.PasswordFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		s.Credentials.Password = val
	}
	if s.Credentials.HostFrom.SecretKeyRef != nil {
		val, err := getSecrectValue(ctx, r, ns, s.Credentials.HostFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		s.Credentials.Host = val
	}
	if s.Credentials.Host != "" {
		return s.Credentials.URL(), nil
	}
	return nil, fmt.Errorf("no target database defined")
}

// URL returns the URL for the database.
func (c *Credentials) URL() *url.URL {
	u := &url.URL{
		Scheme: c.Scheme,
		Path:   c.Database,
	}
	if DriverBySchema(c.Scheme) == "sqlserver" && c.Database != "" {
		u.Path = ""
		if c.Parameters == nil {
			c.Parameters = map[string]string{}
		}
		c.Parameters["database"] = c.Database
	}
	if c.User != "" || c.Password != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	if len(c.Parameters) > 0 {
		qs := url.Values{}
		for k, v := range c.Parameters {
			qs.Set(k, v)
		}
		u.RawQuery = qs.Encode()
	}
	host := c.Host
	if c.Port > 0 {
		host = fmt.Sprintf("%s:%d", host, c.Port)
	}
	u.Host = host
	return u
}

func getSecrectValue(
	ctx context.Context,
	r client.Reader,
	ns string,
	ref *corev1.SecretKeySelector,
) (string, error) {
	val := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, val)
	if err != nil {
		return "", err
	}
	return string(val.Data[ref.Key]), nil
}

// DriverBySchema returns the driver from the given schema.
// it remove the schema modifier if present.
// e.g. mysql+unix -> mysql
// it also handles aliases.
// e.g. mariadb -> mysql
func DriverBySchema(schema string) string {
	p := strings.SplitN(schema, "+", 2)
	switch drv := strings.ToLower(p[0]); drv {
	case "libsql":
		return "sqlite"
	case "maria", "mariadb":
		return "mysql"
	case "postgresql":
		return "postgres"
	case "sqlserver", "azuresql", "mssql":
		return "sqlserver"
	default:
		return drv
	}
}
