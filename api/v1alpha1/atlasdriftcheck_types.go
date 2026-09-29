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

package v1alpha1

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"ariga.io/atlas/atlasexec"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type (
	//+kubebuilder:object:root=true
	//
	// AtlasDriftCheckList contains a list of AtlasDriftCheck
	AtlasDriftCheckList struct {
		metav1.TypeMeta `json:",inline"`
		metav1.ListMeta `json:"metadata,omitempty"`

		Items []AtlasDriftCheck `json:"items"`
	}
	//+kubebuilder:object:root=true
	//+kubebuilder:subresource:status
	//
	// AtlasDriftCheck periodically checks an AtlasMigration database for drift
	// and reports the result in its status. It does not change the database or
	// report the DDL needed to fix the drift.
	// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
	// +kubebuilder:printcolumn:name="Drifted",type=string,JSONPath=`.status.conditions[?(@.type=="Drifted")].status`
	// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
	// +kubebuilder:printcolumn:name="Objects",type=integer,JSONPath=`.status.summary.total`
	// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
	// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
	// +kubebuilder:printcolumn:name="Last Check",type=date,JSONPath=`.status.lastCheckTime`
	// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
	// +kubebuilder:printcolumn:name="Fingerprint",type=string,JSONPath=`.status.fingerprint`,priority=1
	// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.mode`,priority=1
	AtlasDriftCheck struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`

		// Defines the desired state of AtlasDriftCheck.
		Spec AtlasDriftCheckSpec `json:"spec,omitempty"`
		// Reports the observed state of AtlasDriftCheck.
		//+kubebuilder:default={"observedGeneration":-1}
		Status AtlasDriftCheckStatus `json:"status,omitempty"`
	}
	// AtlasDriftCheckSpec defines the desired state of AtlasDriftCheck
	AtlasDriftCheckSpec struct {
		// The AtlasMigration to check for drift.
		TargetRef DriftCheckTarget `json:"targetRef"`
		// How often to check for drift. The minimum is 1m because each check uses
		// the same database lock as migrations, and more frequent checks may conflict
		// with deployments.
		// +kubebuilder:default="5m"
		// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m')",message="interval must be at least 1m"
		Interval metav1.Duration `json:"interval,omitempty"`
		// Maximum time allowed for one drift check.
		// +kubebuilder:default="5m"
		Timeout metav1.Duration `json:"timeout,omitempty"`
		// Stops drift checks without deleting the resource.
		// +optional
		Suspend bool `json:"suspend,omitempty"`
		// How to handle detected drift. Report records the drift in the Drifted
		// condition. Fail also sets Ready=False and Stalled=True, so GitOps tools can
		// treat the check as degraded.
		// +kubebuilder:default=Report
		OnDrift DriftAction `json:"onDrift,omitempty"`
		// Database objects to ignore, such as "public.audit_*" or
		// "*[type=extension]". If empty, uses the exclude list from the target's
		// spec.policy.drift.
		// +optional
		Exclude []string `json:"exclude,omitempty"`
	}
	// DriftCheckTarget references the resource to check.
	DriftCheckTarget struct {
		// Name of the AtlasMigration in the same namespace.
		// +kubebuilder:validation:MinLength=1
		Name string `json:"name"`
	}
	// AtlasDriftCheckStatus defines the observed state of AtlasDriftCheck
	AtlasDriftCheckStatus struct {
		// Generation last processed by the controller.
		// +optional
		ObservedGeneration int64 `json:"observedGeneration,omitempty"`
		// Conditions represent the latest available observations of an object's state.
		// +optional
		Conditions []metav1.Condition `json:"conditions,omitempty"`
		// Applied migration version used to resolve the expected state.
		// +optional
		Version string `json:"version,omitempty"`
		// Identifies the current drift. It changes when the drift changes and is
		// empty when there is no drift.
		// +optional
		Fingerprint string `json:"fingerprint,omitempty"`
		// How the expected state was resolved, "registry" or "local".
		// +optional
		Mode string `json:"mode,omitempty"`
		// Counts drifted objects. It is empty when there is no drift.
		// +optional
		Summary *DriftSummary `json:"summary,omitempty"`
		// Time when the last drift check completed.
		// +optional
		LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`
	}
	// DriftSummary counts the drifted objects by kind and by object type.
	DriftSummary struct {
		// Total number of drifted objects.
		Total int `json:"total"`
		// Number of objects found only in the database.
		// +optional
		Extra int `json:"extra,omitempty"`
		// Number of objects found only in the expected state.
		// +optional
		Missing int `json:"missing,omitempty"`
		// Number of objects that exist in both states but differ.
		// +optional
		Modified int `json:"modified,omitempty"`
		// Drifted objects by type, e.g. {"table": 2, "role": 1}.
		// +optional
		Types map[string]int `json:"types,omitempty"`
	}
)

// DriftAction controls how the check reports detected drift.
// +kubebuilder:validation:Enum=Report;Fail
type DriftAction string

// DriftAction values.
const (
	// DriftActionReport reports the drift on the Drifted condition only.
	DriftActionReport DriftAction = "Report"
	// DriftActionFail also marks the check not ready and stalled.
	DriftActionFail DriftAction = "Fail"
)

func init() {
	SchemeBuilder.Register(&AtlasDriftCheck{}, &AtlasDriftCheckList{})
}

// SetChecked records a completed check that found no drift.
func (c *AtlasDriftCheck) SetChecked(rep *atlasexec.MigrateDrift) {
	c.applyReport(rep)
	msg := fmt.Sprintf("no drift detected%s", atVersion(rep))
	c.setOutcome(ReasonChecked, msg, false)
	c.setCondition(metav1.Condition{
		Type:    driftedCond,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonNoDrift,
		Message: msg,
	})
}

// SetDrifted records a completed check that found drift. The action decides
// whether the drift also marks the check not ready.
func (c *AtlasDriftCheck) SetDrifted(rep *atlasexec.MigrateDrift, action DriftAction) {
	c.applyReport(rep)
	msg := driftMessage(rep)
	c.setCondition(metav1.Condition{
		Type:    driftedCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonDriftDetected,
		Message: msg,
	})
	if action == DriftActionFail {
		c.setOutcome(ReasonDriftDetected, msg, true)
		return
	}
	c.setOutcome(ReasonChecked, msg, false)
}

// SetCheckFailed records a check that could not be completed. A permanent
// failure stalls the resource; a transient one keeps it reconciling. Either
// way the result of the last completed check is kept, and the Drifted
// condition becomes Unknown because it is no longer being observed.
func (c *AtlasDriftCheck) SetCheckFailed(reason, message string, permanent bool) {
	c.Status.ObservedGeneration = c.Generation
	c.setCondition(metav1.Condition{Type: readyCond, Status: metav1.ConditionFalse, Reason: reason, Message: message})
	c.setCondition(metav1.Condition{Type: reconcilingCond, Status: boolCondition(!permanent), Reason: reason, Message: message})
	c.setCondition(metav1.Condition{Type: stalledCond, Status: boolCondition(permanent), Reason: reason, Message: message})
	c.setCondition(metav1.Condition{Type: driftedCond, Status: metav1.ConditionUnknown, Reason: reason, Message: message})
}

// SetTargetNotReady records that the check was skipped because the target is
// mid-apply. It touches the Reconciling condition only, so the result of the
// last completed check stays visible.
func (c *AtlasDriftCheck) SetTargetNotReady(message string) {
	c.Status.ObservedGeneration = c.Generation
	c.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonTargetNotReady,
		Message: message,
	})
}

// SetSuspended records that the checks are suspended.
func (c *AtlasDriftCheck) SetSuspended() {
	c.Status.ObservedGeneration = c.Generation
	c.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonSuspended,
		Message: "checks are suspended",
	})
}

// applyReport stores the outcome of a completed check.
func (c *AtlasDriftCheck) applyReport(rep *atlasexec.MigrateDrift) {
	now := metav1.Now()
	c.Status.ObservedGeneration = c.Generation
	c.Status.Version = rep.Version
	c.Status.Mode = rep.Mode
	c.Status.LastCheckTime = &now
	c.Status.Fingerprint = ""
	c.Status.Summary = nil
	if !rep.Drifted {
		return
	}
	c.Status.Fingerprint = rep.Fingerprint
	if s := rep.Summary; s != nil {
		c.Status.Summary = &DriftSummary{
			Total:    s.Total,
			Extra:    s.Extra,
			Missing:  s.Missing,
			Modified: s.Modified,
			Types:    maps.Clone(s.Types),
		}
	}
}

// setOutcome records a completed check on the standard conditions. A stalled
// outcome is also reported as not ready.
func (c *AtlasDriftCheck) setOutcome(reason, msg string, stalled bool) {
	c.setCondition(metav1.Condition{Type: readyCond, Status: boolCondition(!stalled), Reason: reason, Message: msg})
	c.setCondition(metav1.Condition{Type: reconcilingCond, Status: metav1.ConditionFalse, Reason: reason, Message: msg})
	c.setCondition(metav1.Condition{Type: stalledCond, Status: boolCondition(stalled), Reason: reason, Message: msg})
}

func (c *AtlasDriftCheck) setCondition(cond metav1.Condition) {
	if cond.ObservedGeneration <= 0 {
		cond.ObservedGeneration = c.Generation
	}
	meta.SetStatusCondition(&c.Status.Conditions, cond)
}

// boolCondition maps a boolean to the condition status it stands for.
func boolCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// atVersion returns the " at version X" suffix, empty when the report has no version.
func atVersion(rep *atlasexec.MigrateDrift) string {
	if rep.Version == "" {
		return ""
	}
	return fmt.Sprintf(" at version %s", rep.Version)
}

// driftMessage describes the drift a report found, e.g.
// "3 drifted objects (extra 1, missing 1, modified 1) at version 2: index 1, table 2".
func driftMessage(rep *atlasexec.MigrateDrift) string {
	s := rep.Summary
	if s == nil || s.Total == 0 {
		return fmt.Sprintf("drift detected%s", atVersion(rep))
	}
	var kinds []string
	for _, k := range []struct {
		name string
		n    int
	}{{"extra", s.Extra}, {"missing", s.Missing}, {"modified", s.Modified}} {
		if k.n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d", k.name, k.n))
		}
	}
	word := "drifted objects"
	if s.Total == 1 {
		word = "drifted object"
	}
	msg := fmt.Sprintf("%d %s", s.Total, word)
	if len(kinds) > 0 {
		msg = fmt.Sprintf("%s (%s)", msg, strings.Join(kinds, ", "))
	}
	msg += atVersion(rep)
	if len(s.Types) > 0 {
		types := make([]string, 0, len(s.Types))
		for _, t := range slices.Sorted(maps.Keys(s.Types)) {
			types = append(types, fmt.Sprintf("%s %d", t, s.Types[t]))
		}
		msg = fmt.Sprintf("%s: %s", msg, strings.Join(types, ", "))
	}
	return msg
}
