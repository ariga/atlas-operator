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
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"ariga.io/atlas/atlasexec"
	"ariga.io/atlas/sql/migrate"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
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
	// AtlasExec is the interface for the atlas exec client.
	AtlasExec interface {
		// MigrateApplySlice runs the `migrate apply` command and returns the successful runs.
		MigrateApplySlice(context.Context, *atlasexec.MigrateApplyParams) ([]*atlasexec.MigrateApply, error)
		// MigrateDown runs the `migrate down` command.
		MigrateDown(context.Context, *atlasexec.MigrateDownParams) (*atlasexec.MigrateDown, error)
		// MigrateLint runs the `migrate lint` command.
		MigrateLint(context.Context, *atlasexec.MigrateLintParams) (*atlasexec.SummaryReport, error)
		// MigrateStatus runs the `migrate status` command.
		MigrateStatus(context.Context, *atlasexec.MigrateStatusParams) (*atlasexec.MigrateStatus, error)
		// SchemaApplySlice runs the `schema apply` command and returns the successful runs.
		SchemaApplySlice(context.Context, *atlasexec.SchemaApplyParams) ([]*atlasexec.SchemaApply, error)
		// SchemaInspect runs the `schema inspect` command.
		SchemaInspect(ctx context.Context, params *atlasexec.SchemaInspectParams) (string, error)
		// SchemaPush runs the `schema push` command.
		SchemaPush(context.Context, *atlasexec.SchemaPushParams) (*atlasexec.SchemaPush, error)
		// SchemaPlan runs the `schema plan` command.
		SchemaPlan(context.Context, *atlasexec.SchemaPlanParams) (*atlasexec.SchemaPlan, error)
		// SchemaPlanList runs the `schema plan list` command.
		SchemaPlanList(context.Context, *atlasexec.SchemaPlanListParams) ([]atlasexec.SchemaPlanFile, error)
		// WhoAmI runs the `whoami` command.
		WhoAmI(context.Context, *atlasexec.WhoAmIParams) (*atlasexec.WhoAmI, error)
		// SetStdout specifies a writer to stream stdout to for every command.
		SetStdout(io.Writer)
		// SetStderr specifies a writer to stream stderr to for every command.
		SetStderr(io.Writer)
	}
	// AtlasExecFn is a function that returns an AtlasExec
	// with the working directory.
	AtlasExecFn func(string, *Cloud) (AtlasExec, error)
	// Cloud holds the cloud configuration.
	Cloud struct {
		Token string
		Repo  string
		URL   string
	}
)

// NewAtlasExec returns a new AtlasExec with the given directory and cloud configuration.
// The atlas binary is expected to be in the $PATH.
func NewAtlasExec(dir string, c *Cloud) (AtlasExec, error) {
	cli, err := atlasexec.NewClient(dir, "atlas")
	if err != nil {
		return nil, err
	}
	if c != nil && c.Token != "" {
		env := atlasexec.NewOSEnviron()
		env["ATLAS_TOKEN"] = c.Token
		if err = cli.SetEnv(env); err != nil {
			return nil, err
		}
	}
	return cli, nil
}

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

// isChecksumErr returns true if the error is a checksum error.
func isChecksumErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "checksum mismatch")
}

// isConnectionErr returns true if the error is a connection error.
func isConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection timed out") ||
		strings.Contains(err.Error(), "connection refused")
}

// transientError is an error that should be retried.
type transientError struct {
	err error
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

// transient wraps an error to indicate that it should be retried.
func transient(err error) error {
	return &transientError{err: err}
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
func result(err error, retryAfter time.Duration) (r ctrl.Result, _ error) {
	if t := (*transientError)(nil); errors.As(err, &t) {
		r.RequeueAfter = retryAfter
	}
	return r, nil
}

// listStringVal returns a cty.ListVal from the given slice of strings.
func listStringVal(s []string) cty.Value {
	v := make([]cty.Value, len(s))
	for i, s := range s {
		v[i] = cty.StringVal(s)
	}
	return cty.ListVal(v)
}

// mergeBlocks merges the given block with the env block with the given name.
func mergeBlocks(dst *hclwrite.Body, src *hclwrite.Body, atlasEnvName string) {
	evaluateEnvBlock(dst, atlasEnvName)
	evaluateEnvBlock(src, atlasEnvName)
	for _, srcBlk := range src.Blocks() {
		distBlk := searchBlock(dst, srcBlk)
		// If there is no block with the same type and name, append it.
		if distBlk == nil {
			appendBlock(dst, srcBlk)
			continue
		}
		mergeBlock(distBlk, srcBlk)
	}
}

// searchBlock searches for a block with the given type and name in the parent block.
func searchBlock(parent *hclwrite.Body, target *hclwrite.Block) *hclwrite.Block {
	blocks := parent.Blocks()
	typBlocks := make([]*hclwrite.Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type() == target.Type() {
			typBlocks = append(typBlocks, b)
		}
	}
	if len(typBlocks) == 0 {
		// No things here, return nil.
		return nil
	}
	// Check if there is a block with the given name.
	idx := slices.IndexFunc(typBlocks, func(b *hclwrite.Block) bool {
		return slices.Compare(b.Labels(), target.Labels()) == 0
	})
	if idx == -1 {
		// No block matched, check if there is an unnamed env block.
		idx = slices.IndexFunc(typBlocks, func(b *hclwrite.Block) bool {
			return len(b.Labels()) == 0
		})
		if idx == -1 {
			return nil
		}
	}
	return typBlocks[idx]
}

// mergeBlock merges the source block into the destination block.
func mergeBlock(dst, src *hclwrite.Block) {
	for name, attr := range mapsSorted(src.Body().Attributes()) {
		dst.Body().SetAttributeRaw(name, attr.Expr().BuildTokens(nil))
	}
	// Traverse to the nested blocks.
	mergeBlocks(dst.Body(), src.Body(), "")
}

// appendBlock appends a block to the body and ensures there is a newline before the block.
// It returns the appended block.
//
// There is a bug in hclwrite that causes the block to be appended without a newline
// https://github.com/hashicorp/hcl/issues/687
func appendBlock(body *hclwrite.Body, blk *hclwrite.Block) *hclwrite.Block {
	t := body.BuildTokens(nil)
	if len(t) == 0 || t[len(t)-1].Type != hclsyntax.TokenNewline {
		body.AppendNewline()
	}
	return body.AppendBlock(blk)
}

func evaluateEnvBlock(blocks *hclwrite.Body, atlasEnvName string) error {
	if blocks == nil {
		return nil
	}
	for _, block := range blocks.Blocks() {
		if block.Type() != "env" {
			continue
		}
		if len(block.Labels()) > 0 {
			return nil
		}
		// If the block has no labels, check env name attribute.
		body := block.Body()
		for attrN, attr := range body.Attributes() {
			if attrN == "name" {
				attrV := strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes()))
				// If the name is using "atlas.env", evaluate it to the env label.
				if attrV == "atlas.env" {
					if atlasEnvName == "" {
						return nil
					}
					block.SetLabels([]string{atlasEnvName})
					body.RemoveAttribute(attrN)
				}
			}
		}
	}
	return nil
}

// mapsSorted return a sequence of key-value pairs sorted by key.
func mapsSorted[K cmp.Ordered, V any](m map[K]V) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}

// backoffDelayAt returns the backoff delay at the given retry count.
// Backoff is exponential with base 5.
func backoffDelayAt(retry int) time.Duration {
	return time.Duration(retry) * 5 * time.Second
}
