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
		URLFrom URLFrom `json:"urlFrom,omitempty"`
		// Credentials defines the credentials to use when connecting to the database.
		// Used instead of URL or URLFrom.
		Credentials Credentials `json:"credentials,omitempty"`
	}
	// Credentials defines the credentials to use when connecting to the database.
	Credentials struct {
		Scheme       string            `json:"scheme,omitempty"`
		User         string            `json:"user,omitempty"`
		Password     string            `json:"password,omitempty"`
		PasswordFrom PasswordFrom      `json:"passwordFrom,omitempty"`
		Host         string            `json:"host,omitempty"`
		Port         int               `json:"port,omitempty"`
		Database     string            `json:"database,omitempty"`
		Parameters   map[string]string `json:"parameters,omitempty"`
	}
	// PasswordFrom references a key containing the password.
	PasswordFrom struct {
		// SecretKeyRef defines the secret key reference to use for the password.
		SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	}
	// URLFrom defines a reference to a secret key that contains the Atlas URL of the
	// target database schema.
	URLFrom struct {
		// SecretKeyRef references to the key of a secret in the same namespace.
		SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	}
)

// DatabaseURL returns the database url.
func (s TargetSpec) DatabaseURL(ctx context.Context, r client.Reader, ns string) (*url.URL, error) {
	switch {
	case s.URLFrom.SecretKeyRef != nil:
		val := &corev1.Secret{}
		ref := s.URLFrom.SecretKeyRef
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, val)
		if err != nil {
			return nil, err
		}
		return url.Parse(string(val.Data[ref.Key]))
	case s.URL != "":
		return url.Parse(s.URL)
	case s.Credentials.Host != "":
		// Read the password from the secret if defined.
		if s.Credentials.PasswordFrom.SecretKeyRef != nil {
			val := &corev1.Secret{}
			ref := s.Credentials.PasswordFrom.SecretKeyRef
			err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, val)
			if err != nil {
				return nil, err
			}
			// Set the password.
			s.Credentials.Password = string(val.Data[ref.Key])
		}
		return s.Credentials.URL(), nil
	default:
		return nil, fmt.Errorf("no target database defined")
	}
}

// URL returns the URL for the database.
func (c *Credentials) URL() *url.URL {
	u := &url.URL{
		Scheme: c.Scheme,
		Path:   c.Database,
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
