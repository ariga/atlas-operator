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

package controllers

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/migrate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultEnvName = "kubernetes"

type (
	Manager interface {
		GetClient() client.Client
		GetScheme() *runtime.Scheme
		GetEventRecorderFor(name string) record.EventRecorder
	}
	// AtlasExec is the interface for the Atlas SDK.
	AtlasExec interface {
		MigrateApply(ctx context.Context, params *atlasexec.MigrateApplyParams) (*atlasexec.MigrateApply, error)
		MigrateLint(ctx context.Context, params *atlasexec.MigrateLintParams) (*atlasexec.SummaryReport, error)
		MigrateStatus(ctx context.Context, params *atlasexec.MigrateStatusParams) (*atlasexec.MigrateStatus, error)

		SchemaApply(ctx context.Context, params *atlasexec.SchemaApplyParams) (*atlasexec.SchemaApply, error)
		SchemaInspect(ctx context.Context, params *atlasexec.SchemaInspectParams) (string, error)
	}
	// AtlasExecFn is a function that returns an AtlasExec
	// with the working directory.
	AtlasExecFn func(string) (AtlasExec, error)
)

var (
	//go:embed templates
	tmpls embed.FS
	tmpl  = template.Must(template.New("operator").
		Funcs(template.FuncMap{
			"hclValue": func(s string) string {
				if s == "" {
					return s
				}
				return strings.ReplaceAll(strings.ToUpper(s), "-", "_")
			},
			"slides": func(s []string) string {
				b := &strings.Builder{}
				b.WriteRune('[')
				for i, v := range s {
					if i > 0 {
						b.WriteRune(',')
					}
					fmt.Fprintf(b, "%q", v)
				}
				b.WriteRune(']')
				return b.String()
			},
			"removeSpecialChars": func(s interface{}) (string, error) {
				r := regexp.MustCompile("[\t\r\n]")
				switch s := s.(type) {
				case string:
					return r.ReplaceAllString(s, ""), nil
				case fmt.Stringer:
					return r.ReplaceAllString(s.String(), ""), nil
				default:
					return "", fmt.Errorf("unsupported type %T", s)
				}
			},
		}).
		ParseFS(tmpls, "templates/*.tmpl"),
	)
	sqlErrRegex = regexp.MustCompile(`sql/migrate: (execute: )?executing statement`)
)

func getConfigMap(ctx context.Context, r client.Reader, ns string, ref *corev1.LocalObjectReference) (*corev1.ConfigMap, error) {
	cfg := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, cfg)
	if err != nil {
		return nil, transient(err)
	}
	return cfg, nil
}

// getSecretValue gets the value of the given secret key selector.
func getSecretValue(ctx context.Context, r client.Reader, ns string, selector *corev1.SecretKeySelector) (string, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: selector.Name, Namespace: ns}, secret)
	if err != nil {
		return "", transient(err)
	}
	if _, ok := secret.Data[selector.Key]; !ok {
		return "", fmt.Errorf("secret %s/%s does not contain key %q", ns, selector.Name, selector.Key)
	}
	return string(secret.Data[selector.Key]), nil
}

// memDir creates a memory directory from the given map.
func memDir(m map[string]string) (migrate.Dir, error) {
	f := &migrate.MemDir{}
	for key, value := range m {
		if err := f.WriteFile(key, []byte(value)); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// isSQLErr returns true if the error is a SQL error.
func isSQLErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "executing statement:") || sqlErrRegex.MatchString(s)
}

// isChecksumErr returns true if the error is a checksum error.
func isChecksumErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "checksum mismatch")
}

// transientError is an error that should be retried.
type transientError struct {
	err   error
	after time.Duration
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

// transient wraps an error to indicate that it should be retried.
func transient(err error) error {
	return transientAfter(err, 5*time.Second)
}

// transientAfter wraps an error to indicate that it should be retried after
// the given duration.
func transientAfter(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &transientError{err: err, after: after}
}

func isTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}

// result returns a ctrl.Result and an error. If the error is transient, the
// task will be requeued after seconds defined by the error.
// Permanent errors are not returned as errors because they cause
// the controller to requeue indefinitely. Instead, they should be
// reported as a status condition.
func result(err error) (r ctrl.Result, _ error) {
	if t := (*transientError)(nil); errors.As(err, &t) {
		r.RequeueAfter = t.after
	}
	return r, nil
}
