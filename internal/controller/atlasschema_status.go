// Copyright 2025 The Atlas Operator Authors.
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
	"encoding/json"
	"fmt"

	"ariga.io/atlas-go-sdk/atlasexec"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/result"
	"github.com/ariga/atlas-operator/internal/status"
)

type optionBuilder struct {
	opts []status.Option[*v1alpha1.AtlasSchema]
}

// statusOptions returns an initialized optionBuilder
func statusOptions() *optionBuilder {
	return &optionBuilder{}
}

// GetOptions implements the OptionBuilder interface
func (o *optionBuilder) GetOptions() []status.Option[*v1alpha1.AtlasSchema] {
	return o.opts
}

func (o *optionBuilder) withCondition(condition metav1.Condition) *optionBuilder {
	o.opts = append(o.opts, conditionOption{cond: condition})
	return o
}

func (o *optionBuilder) withObservedHash(h string) *optionBuilder {
	o.opts = append(o.opts, observedHashOption{hash: h})
	return o
}

func (o *optionBuilder) withPlanFile(f *atlasexec.SchemaPlanFile) *optionBuilder {
	o.opts = append(o.opts, planFileOption{file: f})
	return o
}

func (o *optionBuilder) withReport(p *atlasexec.SchemaApply) *optionBuilder {
	// Truncate the applied and pending changes to 1024 bytes.
	p.Changes.Applied = truncateSQL(p.Changes.Applied, sqlLimitSize)
	p.Changes.Pending = truncateSQL(p.Changes.Pending, sqlLimitSize)
	o.opts = append(o.opts, reportOption{report: p})
	if plan := p.Plan; plan != nil {
		return o.withPlanFile(plan.File)
	}
	return o
}

type observedHashOption struct {
	hash string
}

func (o observedHashOption) ApplyOption(obj *v1alpha1.AtlasSchema) {
	obj.Status.ObservedHash = o.hash
}

func (o observedHashOption) GetResult() (reconcile.Result, error) {
	return result.OK()
}

type reportOption struct {
	report *atlasexec.SchemaApply
}

func (m reportOption) ApplyOption(obj *v1alpha1.AtlasSchema) {
	var msg string
	if j, err := json.Marshal(m.report); err != nil {
		msg = fmt.Sprintf("Error marshalling apply response: %v", err)
	} else {
		msg = fmt.Sprintf("The schema has been applied successfully. Apply response: %s", j)
	}
	obj.Status.LastApplied = m.report.End.Unix()
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "Applied",
		Message: msg,
	})
}

func (m reportOption) GetResult() (reconcile.Result, error) {
	return result.OK()
}

func (o *optionBuilder) withNotReady(reason string, err error) *optionBuilder {
	err = IgnoreNonTransient(err)
	o.opts = append(o.opts, conditionOption{
		cond: metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: err.Error(),
		},
		err: err,
	})
	return o
}

type planFileOption struct {
	file *atlasexec.SchemaPlanFile
}

func (m planFileOption) ApplyOption(obj *v1alpha1.AtlasSchema) {
	obj.Status.PlanURL = m.file.URL
	obj.Status.PlanLink = m.file.Link
}

func (m planFileOption) GetResult() (reconcile.Result, error) {
	return result.OK()
}

type conditionOption struct {
	cond metav1.Condition
	err  error
}

func (m conditionOption) ApplyOption(obj *v1alpha1.AtlasSchema) {
	meta.SetStatusCondition(&obj.Status.Conditions, m.cond)
}

func (m conditionOption) GetResult() (reconcile.Result, error) {
	return result.Transient(m.err)
}

func IgnoreNonTransient(err error) error {
	if err == nil || result.IsTransient(err) {
		return nil
	}
	return err
}
