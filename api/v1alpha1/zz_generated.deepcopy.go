//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import (
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasMigration) DeepCopyInto(out *AtlasMigration) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasMigration.
func (in *AtlasMigration) DeepCopy() *AtlasMigration {
	if in == nil {
		return nil
	}
	out := new(AtlasMigration)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AtlasMigration) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasMigrationList) DeepCopyInto(out *AtlasMigrationList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]AtlasMigration, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasMigrationList.
func (in *AtlasMigrationList) DeepCopy() *AtlasMigrationList {
	if in == nil {
		return nil
	}
	out := new(AtlasMigrationList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AtlasMigrationList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasMigrationSpec) DeepCopyInto(out *AtlasMigrationSpec) {
	*out = *in
	in.URLFrom.DeepCopyInto(&out.URLFrom)
	in.Cloud.DeepCopyInto(&out.Cloud)
	in.Dir.DeepCopyInto(&out.Dir)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasMigrationSpec.
func (in *AtlasMigrationSpec) DeepCopy() *AtlasMigrationSpec {
	if in == nil {
		return nil
	}
	out := new(AtlasMigrationSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasMigrationStatus) DeepCopyInto(out *AtlasMigrationStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasMigrationStatus.
func (in *AtlasMigrationStatus) DeepCopy() *AtlasMigrationStatus {
	if in == nil {
		return nil
	}
	out := new(AtlasMigrationStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasSchema) DeepCopyInto(out *AtlasSchema) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasSchema.
func (in *AtlasSchema) DeepCopy() *AtlasSchema {
	if in == nil {
		return nil
	}
	out := new(AtlasSchema)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AtlasSchema) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasSchemaList) DeepCopyInto(out *AtlasSchemaList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]AtlasSchema, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasSchemaList.
func (in *AtlasSchemaList) DeepCopy() *AtlasSchemaList {
	if in == nil {
		return nil
	}
	out := new(AtlasSchemaList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AtlasSchemaList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasSchemaSpec) DeepCopyInto(out *AtlasSchemaSpec) {
	*out = *in
	in.URLFrom.DeepCopyInto(&out.URLFrom)
	in.Schema.DeepCopyInto(&out.Schema)
	if in.Exclude != nil {
		in, out := &in.Exclude, &out.Exclude
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	out.Policy = in.Policy
	if in.Schemas != nil {
		in, out := &in.Schemas, &out.Schemas
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasSchemaSpec.
func (in *AtlasSchemaSpec) DeepCopy() *AtlasSchemaSpec {
	if in == nil {
		return nil
	}
	out := new(AtlasSchemaSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AtlasSchemaStatus) DeepCopyInto(out *AtlasSchemaStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AtlasSchemaStatus.
func (in *AtlasSchemaStatus) DeepCopy() *AtlasSchemaStatus {
	if in == nil {
		return nil
	}
	out := new(AtlasSchemaStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *CheckConfig) DeepCopyInto(out *CheckConfig) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new CheckConfig.
func (in *CheckConfig) DeepCopy() *CheckConfig {
	if in == nil {
		return nil
	}
	out := new(CheckConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Cloud) DeepCopyInto(out *Cloud) {
	*out = *in
	in.TokenFrom.DeepCopyInto(&out.TokenFrom)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Cloud.
func (in *Cloud) DeepCopy() *Cloud {
	if in == nil {
		return nil
	}
	out := new(Cloud)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Diff) DeepCopyInto(out *Diff) {
	*out = *in
	out.Skip = in.Skip
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Diff.
func (in *Diff) DeepCopy() *Diff {
	if in == nil {
		return nil
	}
	out := new(Diff)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Dir) DeepCopyInto(out *Dir) {
	*out = *in
	if in.ConfigMapRef != nil {
		in, out := &in.ConfigMapRef, &out.ConfigMapRef
		*out = new(v1.LocalObjectReference)
		**out = **in
	}
	out.Remote = in.Remote
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Dir.
func (in *Dir) DeepCopy() *Dir {
	if in == nil {
		return nil
	}
	out := new(Dir)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Lint) DeepCopyInto(out *Lint) {
	*out = *in
	out.Destructive = in.Destructive
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Lint.
func (in *Lint) DeepCopy() *Lint {
	if in == nil {
		return nil
	}
	out := new(Lint)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Policy) DeepCopyInto(out *Policy) {
	*out = *in
	out.Lint = in.Lint
	out.Diff = in.Diff
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Policy.
func (in *Policy) DeepCopy() *Policy {
	if in == nil {
		return nil
	}
	out := new(Policy)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Remote) DeepCopyInto(out *Remote) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Remote.
func (in *Remote) DeepCopy() *Remote {
	if in == nil {
		return nil
	}
	out := new(Remote)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Schema) DeepCopyInto(out *Schema) {
	*out = *in
	if in.ConfigMapKeyRef != nil {
		in, out := &in.ConfigMapKeyRef, &out.ConfigMapKeyRef
		*out = new(v1.ConfigMapKeySelector)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Schema.
func (in *Schema) DeepCopy() *Schema {
	if in == nil {
		return nil
	}
	out := new(Schema)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SkipChanges) DeepCopyInto(out *SkipChanges) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SkipChanges.
func (in *SkipChanges) DeepCopy() *SkipChanges {
	if in == nil {
		return nil
	}
	out := new(SkipChanges)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *TokenFrom) DeepCopyInto(out *TokenFrom) {
	*out = *in
	if in.SecretKeyRef != nil {
		in, out := &in.SecretKeyRef, &out.SecretKeyRef
		*out = new(v1.SecretKeySelector)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new TokenFrom.
func (in *TokenFrom) DeepCopy() *TokenFrom {
	if in == nil {
		return nil
	}
	out := new(TokenFrom)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *URLFrom) DeepCopyInto(out *URLFrom) {
	*out = *in
	if in.SecretKeyRef != nil {
		in, out := &in.SecretKeyRef, &out.SecretKeyRef
		*out = new(v1.SecretKeySelector)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new URLFrom.
func (in *URLFrom) DeepCopy() *URLFrom {
	if in == nil {
		return nil
	}
	out := new(URLFrom)
	in.DeepCopyInto(out)
	return out
}
