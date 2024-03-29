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

import "context"

type versionCtxKey struct{}

// WithVersionContext returns a new context with the given verison.
func WithVersionContext(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, versionCtxKey{}, version)
}

// VersionFromContext returns the version from the given context.
func VersionFromContext(ctx context.Context) string {
	if v := ctx.Value(versionCtxKey{}); v != nil {
		return v.(string)
	}
	return ""
}
