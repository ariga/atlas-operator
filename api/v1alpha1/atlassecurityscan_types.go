// Copyright 2026 The Atlas Operator Authors.
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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type (
	//+kubebuilder:object:root=true
	//
	// AtlasSecurityScanList contains a list of AtlasSecurityScan
	AtlasSecurityScanList struct {
		metav1.TypeMeta `json:",inline"`
		metav1.ListMeta `json:"metadata,omitempty"`

		Items []AtlasSecurityScan `json:"items"`
	}
	//+kubebuilder:object:root=true
	//+kubebuilder:subresource:status
	//
	// AtlasSecurityScan is the Schema for the atlassecurityscans API
	// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
	// +kubebuilder:printcolumn:name="Secure",type=string,JSONPath=`.status.conditions[?(@.type=="Secure")].status`
	// +kubebuilder:printcolumn:name="Issues",type=integer,JSONPath=`.status.issues`
	// +kubebuilder:printcolumn:name="Level",type=string,JSONPath=`.status.levels[0].level`
	// +kubebuilder:printcolumn:name="Last Scan",type=date,JSONPath=`.status.lastScanTime`
	// +kubebuilder:printcolumn:name="Next Scan",type=date,JSONPath=`.status.nextScanTime`
	AtlasSecurityScan struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`

		Spec AtlasSecurityScanSpec `json:"spec,omitempty"`
		//+kubebuilder:default={"observedGeneration":-1}
		Status AtlasSecurityScanStatus `json:"status,omitempty"`
	}
	// AtlasSecurityScanStatus defines the observed state of AtlasSecurityScan
	AtlasSecurityScanStatus struct {
		// ObservedGeneration is the generation last processed by the controller.
		// +optional
		ObservedGeneration int64 `json:"observedGeneration,omitempty"`
		// Conditions represent the latest available observations of an object's state.
		// +optional
		Conditions []metav1.Condition `json:"conditions,omitempty"`
		// ObservedHash is the hash of the input of the most recent scan.
		// +optional
		ObservedHash string `json:"observedHash,omitempty"`
		// LastScanTime is the time the most recent scan ended.
		// +optional
		LastScanTime *metav1.Time `json:"lastScanTime,omitempty"`
		// LastScanTrigger is what caused the most recent scan to run.
		// +optional
		LastScanTrigger ScanTrigger `json:"lastScanTrigger,omitempty"`
		// NextScanTime is when the next scheduled scan is due. Unset when the
		// resource has no schedule.
		// +optional
		NextScanTime *metav1.Time `json:"nextScanTime,omitempty"`
		// Issues is the number of security issues reported by the most recent scan.
		// +optional
		// +kubebuilder:default=0
		Issues int `json:"issues"`
		// Levels is the number of issues per severity level, highest first,
		// omitting the levels with no issues.
		// +optional
		Levels []SecurityScanLevel `json:"levels,omitempty"`
		// Targets are the databases the most recent scan inspected, with the
		// issues reported for each.
		// +optional
		Targets []SecurityScanTarget `json:"targets,omitempty"`
		// Failed is the number of times the scan has failed.
		// +optional
		// +kubebuilder:default=0
		Failed int `json:"failed"`
	}
	// AtlasSecurityScanSpec defines the desired state of AtlasSecurityScan
	AtlasSecurityScanSpec struct {
		TargetSpec        `json:",inline"`
		ProjectConfigSpec `json:",inline"`
		// Cloud defines the Atlas Cloud configuration. The scan reports from the
		// Atlas Security Graph, hence it requires a token of an organization whose
		// plan includes it.
		Cloud SecurityScanCloud `json:"cloud,omitempty"`
		// Schedule is a cron expression saying when to scan. e.g., "0 6 * * *".
		// Optional: without it the resource is scanned only when something changes.
		// +optional
		Schedule string `json:"schedule,omitempty"`
		// TimeZone the schedule is expressed in, as an IANA name. e.g., "Europe/Paris".
		// Defaults to UTC.
		// +kubebuilder:default=UTC
		// +optional
		TimeZone string `json:"timeZone,omitempty"`
		// TriggerOn lists the Atlas resources in this namespace whose applies scan
		// the database. Optional: without it the resource is scanned only on its
		// schedule.
		// +optional
		TriggerOn []TriggerRef `json:"triggerOn,omitempty"`
		// MinSeverity is the lowest severity to report. e.g., HIGH reports only the
		// HIGH and CRITICAL issues. All issues are reported when unset.
		// +optional
		MinSeverity SecurityLevel `json:"minSeverity,omitempty"`
		// FailOn is the lowest severity at which a reported issue leaves the
		// resource not ready. When unset, reported issues do not affect its
		// readiness; only a database that could not be scanned does.
		// +optional
		FailOn SecurityLevel `json:"failOn,omitempty"`
		// Ignore lists the CVE identifiers not to report. e.g., CVE-2017-18359.
		// +optional
		Ignore []string `json:"ignore,omitempty"`
		// BackoffLimit is the number of retries on error.
		// +kubebuilder:default=20
		BackoffLimit int `json:"backoffLimit,omitempty"`
	}
	// TriggerRef refers to an Atlas resource whose applies trigger a scan.
	TriggerRef struct {
		// Kind of the resource.
		// +kubebuilder:validation:Enum=AtlasSchema;AtlasMigration
		Kind string `json:"kind"`
		// Name of the resource, in the namespace of the scan.
		// +kubebuilder:validation:MinLength=1
		Name string `json:"name"`
	}
	// SecurityScanCloud defines the Atlas Cloud configuration of a security scan.
	SecurityScanCloud struct {
		// TokenFrom defines the reference to the secret key that contains the Atlas Cloud Token.
		TokenFrom TokenFrom `json:"tokenFrom,omitempty"`
	}
	// SecurityScanLevel is the number of issues reported for a severity level.
	SecurityScanLevel struct {
		// Level is the severity, as the Security Graph grades it.
		Level string `json:"level"`
		// Count is the number of issues reported at this level.
		Count int `json:"count"`
	}
	// SecurityScanTarget holds the security issues reported for a scanned database.
	SecurityScanTarget struct {
		// URL of the database, with its credentials redacted.
		URL string `json:"url"`
		// Driver of the database. e.g., postgres.
		// +optional
		Driver string `json:"driver,omitempty"`
		// Version of the database engine. e.g., 16.2.
		// +optional
		Version string `json:"version,omitempty"`
		// Extensions installed in the database.
		// +optional
		Extensions []string `json:"extensions,omitempty"`
		// Vulnerabilities reported for the installed extensions.
		// +optional
		Vulnerabilities []SecurityVulnerability `json:"vulnerabilities,omitempty"`
		// Error is set if the database could not be scanned.
		// +optional
		Error string `json:"error,omitempty"`
	}
	// SecurityVulnerability is a vulnerability reported for an installed extension.
	SecurityVulnerability struct {
		// ID of the vulnerability. e.g., CVE-2024-10977.
		ID string `json:"id"`
		// Name of the extension the vulnerability was reported for.
		Name string `json:"name"`
		// Version of the extension that is installed.
		// +optional
		Version string `json:"version,omitempty"`
		// Level is the severity, as the Security Graph grades it.
		Level string `json:"level"`
		// Severity is the CVSS rating of the record, if it carries one.
		// +optional
		Severity string `json:"severity,omitempty"`
		// Title of the record, if it carries one.
		// +optional
		Title string `json:"title,omitempty"`
		// Suggestion to resolve the vulnerability. e.g., the version to upgrade to.
		// +optional
		Suggestion string `json:"suggestion,omitempty"`
	}
	// SecurityLevel is a severity level, as the Security Graph grades an issue.
	// +kubebuilder:validation:Enum=NORMAL;ELEVATED;HIGH;CRITICAL
	SecurityLevel string
	// ScanTrigger is what caused a scan to run.
	// +kubebuilder:validation:Enum=Schedule;Change;SpecChange
	ScanTrigger string
)

// ScanTrigger values.
const (
	// TriggerSchedule is a scan the schedule was due for.
	TriggerSchedule ScanTrigger = "Schedule"
	// TriggerChange is a scan a watched resource's apply caused.
	TriggerChange ScanTrigger = "Change"
	// TriggerSpecChange is a scan an edit to this resource caused.
	TriggerSpecChange ScanTrigger = "SpecChange"
)

// SecurityLevel values, lowest first.
const (
	SecurityLevelNormal   SecurityLevel = "NORMAL"
	SecurityLevelElevated SecurityLevel = "ELEVATED"
	SecurityLevelHigh     SecurityLevel = "HIGH"
	SecurityLevelCritical SecurityLevel = "CRITICAL"
)

// secureCond reports whether the findings are within the threshold the resource
// declares in failOn. It is orthogonal to readyCond, which only reports whether
// the operator managed to produce a report at all.
const secureCond = "Secure"

func init() {
	SchemeBuilder.Register(&AtlasSecurityScan{}, &AtlasSecurityScanList{})
}

// NamespacedName returns the namespaced name of the object.
func (s *AtlasSecurityScan) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      s.Name,
		Namespace: s.Namespace,
	}
}

// IsReady returns true if the ready condition is true.
func (s *AtlasSecurityScan) IsReady() bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, readyCond)
}

// IsHashModified returns true if the hash is different from the observed hash.
func (s *AtlasSecurityScan) IsHashModified(hash string) bool {
	return hash != s.Status.ObservedHash
}

// SetReconciling sets the ready condition to false with the reason "Reconciling".
func (s *AtlasSecurityScan) SetReconciling(message string) {
	s.Status.ObservedGeneration = s.Generation
	s.setCondition(metav1.Condition{
		Type:    readyCond,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonReconciling,
		Message: message,
	})
	s.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonReconciling,
		Message: message,
	})
	s.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonReconciling,
	})
	s.ResetFailed()
}

// SetReady sets the Ready condition to true, with the summary of the scan as its message.
func (s *AtlasSecurityScan) SetReady(message string) {
	s.Status.ObservedGeneration = s.Generation
	s.setCondition(metav1.Condition{
		Type:    readyCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonScanned,
		Message: message,
	})
	s.setCondition(metav1.Condition{
		Type:   reconcilingCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonScanned,
	})
	s.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: ReasonScanned,
	})
	s.ResetFailed()
}

// SetSecure records whether the scan stayed within failOn. It is only meaningful
// when the resource sets a threshold, and a scan that could not run leaves the
// previous verdict in place rather than resetting it.
func (s *AtlasSecurityScan) SetSecure(secure bool, message string) {
	cond := metav1.Condition{
		Type:    secureCond,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonScanned,
		Message: message,
	}
	if !secure {
		cond.Status, cond.Reason = metav1.ConditionFalse, ReasonSecurityIssues
	}
	s.setCondition(cond)
}

// IsSecure returns true if the most recent scan stayed within failOn.
func (s *AtlasSecurityScan) IsSecure() bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, secureCond)
}

// SetNotReady sets the Ready condition to false
// with the given reason and message.
func (s *AtlasSecurityScan) SetNotReady(reason, message string) {
	s.Status.ObservedGeneration = s.Generation
	s.setCondition(metav1.Condition{
		Type:    readyCond,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	if isFailedReason(reason) {
		s.setCondition(metav1.Condition{
			Type:    reconcilingCond,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		s.setCondition(metav1.Condition{
			Type:    stalledCond,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
		s.IncrementFailed()
		return
	}
	s.setCondition(metav1.Condition{
		Type:    reconcilingCond,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	s.setCondition(metav1.Condition{
		Type:   stalledCond,
		Status: metav1.ConditionFalse,
		Reason: reason,
	})
	// The scan itself succeeded, so the backoff of the errors before it is done.
	s.ResetFailed()
}

// IncrementFailed increments the failed count.
func (s *AtlasSecurityScan) IncrementFailed() {
	s.Status.Failed++
}

// ResetFailed resets the failed count.
func (s *AtlasSecurityScan) ResetFailed() {
	s.Status.Failed = 0
}

// IsExceedBackoffLimit returns true if the failed count exceeds the backoff limit.
func (s *AtlasSecurityScan) IsExceedBackoffLimit() bool {
	return s.Spec.BackoffLimit > 0 && s.Status.Failed > s.Spec.BackoffLimit
}

func (s *AtlasSecurityScan) setCondition(cond metav1.Condition) {
	if cond.ObservedGeneration <= 0 {
		cond.ObservedGeneration = s.Generation
	}
	meta.SetStatusCondition(&s.Status.Conditions, cond)
}
