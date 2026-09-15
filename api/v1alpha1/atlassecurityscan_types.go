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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SecurityLevel is a Security Graph grade. Uppercase mirrors the CLI flags and atlas.hcl deliberately.
// +kubebuilder:validation:Enum=NORMAL;ELEVATED;HIGH;CRITICAL
type SecurityLevel string

// ScanTrigger says why a scan ran.
// +kubebuilder:validation:Enum=Spec;Manual;Apply;Schedule;Policy
type ScanTrigger string

// ScanResult is the outcome of a scan attempt.
// +kubebuilder:validation:Enum=Succeeded;Failed
type ScanResult string

// ScanTriggerKind is a kind whose applies can trigger a scan.
// +kubebuilder:validation:Enum=AtlasSchema;AtlasMigration
type ScanTriggerKind string

// SecurityLevel values, lowest first.
const (
	SecurityLevelNormal   SecurityLevel = "NORMAL"
	SecurityLevelElevated SecurityLevel = "ELEVATED"
	SecurityLevelHigh     SecurityLevel = "HIGH"
	SecurityLevelCritical SecurityLevel = "CRITICAL"
)

// SecurityLevels lists the levels lowest first, the order the policy compares them in.
var SecurityLevels = []SecurityLevel{SecurityLevelNormal, SecurityLevelElevated, SecurityLevelHigh, SecurityLevelCritical}

// ScanTrigger values, in precedence order: one scan satisfies everything pending,
// and the label names the highest-precedence cause.
const (
	TriggerSpec     ScanTrigger = "Spec"
	TriggerManual   ScanTrigger = "Manual"
	TriggerApply    ScanTrigger = "Apply"
	TriggerSchedule ScanTrigger = "Schedule"
	TriggerPolicy   ScanTrigger = "Policy"
)

// ScanResult values.
const (
	ScanSucceeded ScanResult = "Succeeded"
	ScanFailed    ScanResult = "Failed"
)

// ScanTriggerKind values.
const (
	TriggerKindSchema    ScanTriggerKind = "AtlasSchema"
	TriggerKindMigration ScanTriggerKind = "AtlasMigration"
)

const (
	// AnnotationScanRequestedAt requests a scan. Any new value triggers one scan; the value
	// is echoed to status.lastHandledScanRequest when that scan completes successfully.
	AnnotationScanRequestedAt = "db.atlasgo.io/scan-requested-at"
	// LabelSecurityScan names the scan an AtlasSecurityReport belongs to.
	LabelSecurityScan = "db.atlasgo.io/scan"
	// compliantCond is the policy verdict as of the last successful scan. It is
	// orthogonal to readyCond, which only says whether the controller did its job.
	compliantCond = "Compliant"
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
	// AtlasSecurityScan scans a database with `atlas security scan` on a schedule and after the
	// referenced AtlasSchema/AtlasMigration resources apply changes to it.
	// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
	// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
	// +kubebuilder:printcolumn:name="Compliant",type=string,JSONPath=`.status.conditions[?(@.type=="Compliant")].status`
	// +kubebuilder:printcolumn:name="Findings",type=integer,JSONPath=`.status.summary.total`
	// +kubebuilder:printcolumn:name="Highest",type=string,JSONPath=`.status.summary.highestLevel`
	// +kubebuilder:printcolumn:name="Last Scan",type=date,JSONPath=`.status.lastSuccessfulTime`
	// +kubebuilder:printcolumn:name="Next Scan",type=string,JSONPath=`.status.nextScheduleTime`
	// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
	// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.status.lastScan.trigger`,priority=1
	// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`,priority=1
	// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`,priority=1
	AtlasSecurityScan struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`

		Spec AtlasSecurityScanSpec `json:"spec,omitempty"`
		//+kubebuilder:default={"observedGeneration":-1}
		Status AtlasSecurityScanStatus `json:"status,omitempty"`
	}
	// AtlasSecurityScanSpec defines the desired state of AtlasSecurityScan.
	// +kubebuilder:validation:XValidation:rule="(has(self.schedule) && self.schedule != '') || (has(self.triggers) && size(self.triggers) > 0)",message="at least one of spec.schedule or spec.triggers must be set"
	// +kubebuilder:validation:XValidation:rule="!has(self.schedule) || !(self.schedule.startsWith('TZ=') || self.schedule.startsWith('CRON_TZ='))",message="use spec.timeZone instead of a TZ=/CRON_TZ= prefix"
	// +kubebuilder:validation:XValidation:rule="!has(self.schedule) || !self.schedule.startsWith('@every')",message="@every is interval-based and drifts; use a cron expression or @hourly/@daily/@weekly/@monthly/@yearly"
	// +kubebuilder:validation:XValidation:rule="!has(self.timeZone) || self.timeZone != 'Local'",message="timeZone must be an IANA zone name"
	// +kubebuilder:validation:XValidation:rule="!has(self.cloud) || !has(self.cloud.repo)",message="cloud.repo is not used by security scans"
	AtlasSecurityScanSpec struct {
		TargetSpec        `json:",inline"`
		ProjectConfigSpec `json:",inline"`
		// Cloud defines the Atlas Cloud configuration. The Security Graph requires Atlas Pro.
		// +optional
		Cloud Cloud `json:"cloud,omitempty"`
		// Schedule is a cron expression (5 fields, or @hourly/@daily/@weekly/@monthly/@yearly) evaluated in TimeZone.
		// +optional
		// +kubebuilder:validation:MinLength=1
		Schedule string `json:"schedule,omitempty"`
		// TimeZone is the IANA name of the zone the schedule is evaluated in. Defaults to UTC.
		// +optional
		// +kubebuilder:default="UTC"
		// +kubebuilder:validation:MinLength=1
		TimeZone string `json:"timeZone,omitempty"`
		// Triggers are resources in this namespace whose applies cause a scan.
		// +optional
		// +listType=map
		// +listMapKey=kind
		// +listMapKey=name
		// +kubebuilder:validation:MaxItems=32
		Triggers []ScanTriggerRef `json:"triggers,omitempty"`
		// Suspend pauses scanning. Everything that becomes due while suspended is covered by one scan on resume.
		// +optional
		Suspend *bool `json:"suspend,omitempty"`
		// Policy controls which findings are reported and which make the database non-compliant.
		// +optional
		Policy *ScanPolicy `json:"policy,omitempty"`
		// BackoffLimit is the number of retries of a failed scan before the resource stalls. 0 means unlimited.
		// int rather than int32, and defaulted to 20, for parity with the existing kinds.
		// +kubebuilder:default=20
		// +kubebuilder:validation:Minimum=0
		BackoffLimit int `json:"backoffLimit,omitempty"`
	}
	// ScanTriggerRef references an AtlasSchema or AtlasMigration in the same namespace.
	ScanTriggerRef struct {
		Kind ScanTriggerKind `json:"kind"`
		// +kubebuilder:validation:MinLength=1
		// +kubebuilder:validation:MaxLength=253
		Name string `json:"name"`
	}
	// ScanPolicy grades the findings of a scan.
	// +kubebuilder:validation:XValidation:rule="!has(self.failOn) || !has(self.minSeverity) || {'NORMAL':0,'ELEVATED':1,'HIGH':2,'CRITICAL':3}[self.failOn] >= {'NORMAL':0,'ELEVATED':1,'HIGH':2,'CRITICAL':3}[self.minSeverity]",message="failOn must be at or above minSeverity"
	ScanPolicy struct {
		// MinSeverity is the lowest level that is reported (--min-severity).
		// +kubebuilder:default=NORMAL
		MinSeverity SecurityLevel `json:"minSeverity,omitempty"`
		// FailOn is the lowest level at which a non-waived finding makes Compliant=False. Unset: report only.
		// +optional
		FailOn *SecurityLevel `json:"failOn,omitempty"`
		// Ignore lists vulnerabilities that do not count toward the policy. They remain in the report, marked waived.
		// +optional
		// +listType=map
		// +listMapKey=id
		// +kubebuilder:validation:MaxItems=256
		Ignore []IgnoredVulnerability `json:"ignore,omitempty"`
	}
	// IgnoredVulnerability is a waiver for one vulnerability.
	IgnoredVulnerability struct {
		// ID of the vulnerability, e.g. CVE-2024-10977.
		// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$`
		ID     string `json:"id"`
		Waiver `json:",inline"`
	}
	// Waiver documents why and until when a vulnerability is ignored.
	Waiver struct {
		// Reason documents why the vulnerability is waived.
		// +kubebuilder:validation:MinLength=1
		// +kubebuilder:validation:MaxLength=1024
		Reason string `json:"reason"`
		// ExpirationTime is when the waiver stops applying. A scan runs when it passes.
		// +optional
		ExpirationTime *metav1.Time `json:"expirationTime,omitempty"`
	}

	// AtlasSecurityScanStatus defines the observed state of AtlasSecurityScan.
	AtlasSecurityScanStatus struct {
		// +optional
		ObservedGeneration int64 `json:"observedGeneration,omitempty"`
		// +optional
		// +listType=map
		// +listMapKey=type
		Conditions []metav1.Condition `json:"conditions,omitempty"`
		// LastScan describes the most recent attempt, whether it succeeded or failed.
		// +optional
		LastScan *ScanAttempt `json:"lastScan,omitempty"`
		// LastSuccessfulTime is when the most recent successful scan completed. Summary, report and the
		// Compliant condition describe that scan.
		// +optional
		LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
		// LastScheduleTime is the most recent schedule slot at or before the start of a successful scan.
		// +optional
		LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
		// NextScheduleTime is the slot after LastScheduleTime. Omitted while suspended or without a schedule.
		// It advances only when a scan succeeds, so it stays at a missed slot until the catch-up completes.
		// +optional
		NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
		// LastHandledScanRequest is the scan-requested-at annotation value whose scan completed successfully.
		// +optional
		LastHandledScanRequest string `json:"lastHandledScanRequest,omitempty"`
		// Triggers holds the revision of each trigger observed when the last successful scan started.
		// +optional
		// +listType=map
		// +listMapKey=kind
		// +listMapKey=name
		Triggers []ObservedTrigger `json:"triggers,omitempty"`
		// ActiveWaivers lists the waiver ids that were in force when the last successful scan started.
		// +optional
		// +listType=set
		ActiveWaivers []string `json:"activeWaivers,omitempty"`
		// Summary of the last successful scan.
		// +optional
		Summary *ScanSummary `json:"summary,omitempty"`
		// ReportRef names the AtlasSecurityReport holding the findings of the last successful scan.
		// +optional
		ReportRef *corev1.LocalObjectReference `json:"reportRef,omitempty"`
		// Failed is the number of consecutive failed attempts since the last success. int for parity with the existing kinds.
		// +optional
		// +kubebuilder:default=0
		Failed int `json:"failed"`
	}
	// ScanAttempt describes one scan attempt.
	ScanAttempt struct {
		Trigger        ScanTrigger  `json:"trigger"`
		TriggeredBy    string       `json:"triggeredBy,omitempty"`
		StartTime      metav1.Time  `json:"startTime"`
		CompletionTime *metav1.Time `json:"completionTime,omitempty"`
		Result         ScanResult   `json:"result,omitempty"`
		// Message is a fixed description of a failure class, never CLI or driver output.
		Message string `json:"message,omitempty"`
		// InputsHash identifies what was observed when the attempt started: generation, requested-at value,
		// trigger uids and revisions, the schedule slot, and the active waivers. A stalled resource makes one
		// attempt per distinct hash.
		InputsHash string `json:"inputsHash,omitempty"`
	}
	// ObservedTrigger is the revision of a trigger when a scan started.
	ObservedTrigger struct {
		Kind     ScanTriggerKind `json:"kind"`
		Name     string          `json:"name"`
		UID      types.UID       `json:"uid"`
		Revision string          `json:"revision"`
	}
	// ScanSummary counts the findings of a scan. Levels always lists all four, so
	// metric series never disappear.
	ScanSummary struct {
		Driver     string `json:"driver,omitempty"`
		Extensions int32  `json:"extensions"`
		Total      int32  `json:"total"`
		Waived     int32  `json:"waived"`
		// +optional
		HighestLevel SecurityLevel `json:"highestLevel,omitempty"`
		// +listType=map
		// +listMapKey=level
		Levels []LevelCount `json:"levels"`
	}
	// LevelCount is the number of non-waived findings at one level.
	LevelCount struct {
		Level SecurityLevel `json:"level"`
		Count int32         `json:"count"`
	}
)

func init() {
	SchemeBuilder.Register(&AtlasSecurityScan{}, &AtlasSecurityScanList{})
}

// NamespacedName returns the namespaced name of the object.
func (s *AtlasSecurityScan) NamespacedName() types.NamespacedName {
	return types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
}

// IsSuspended reports whether scanning is paused.
func (s *AtlasSecurityScan) IsSuspended() bool {
	return s.Spec.Suspend != nil && *s.Spec.Suspend
}

// IsReady returns true if the ready condition is true.
func (s *AtlasSecurityScan) IsReady() bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, readyCond)
}

// IsStalled reports whether the resource stalled for the given reason, or for any
// reason when none is given.
func (s *AtlasSecurityScan) IsStalled(reason string) bool {
	c := meta.FindStatusCondition(s.Status.Conditions, stalledCond)
	return c != nil && c.Status == metav1.ConditionTrue && (reason == "" || c.Reason == reason)
}

// WasSuspended reports whether the last pass left the resource suspended.
func (s *AtlasSecurityScan) WasSuspended() bool {
	c := meta.FindStatusCondition(s.Status.Conditions, reconcilingCond)
	return c != nil && c.Reason == ReasonSuspended
}

// Compliant returns the policy verdict condition, nil before the first visit.
func (s *AtlasSecurityScan) Compliant() *metav1.Condition {
	return meta.FindStatusCondition(s.Status.Conditions, compliantCond)
}

// SetFirstVisit writes the initial conditions. Nothing is known yet.
func (s *AtlasSecurityScan) SetFirstVisit() {
	s.setConditions(metav1.ConditionUnknown, ReasonReconciling, "Reconciling", true)
	s.SetCompliant(metav1.ConditionUnknown, ReasonNotScanned, "no successful scan yet")
}

// SetScanning marks the resource as converging on its spec while a scan runs.
// Ready moves to Unknown only until the first success; afterwards the last
// result stands while a Spec-triggered re-scan runs.
func (s *AtlasSecurityScan) SetScanning() {
	if s.Status.LastSuccessfulTime == nil {
		s.setCondition(metav1.Condition{Type: readyCond, Status: metav1.ConditionUnknown, Reason: ReasonScanning, Message: "the first scan is running"})
	}
	s.setCondition(metav1.Condition{Type: reconcilingCond, Status: metav1.ConditionTrue, Reason: ReasonScanning, Message: "a scan is running"})
	s.setCondition(metav1.Condition{Type: stalledCond, Status: metav1.ConditionFalse, Reason: ReasonScanning})
}

// SetScanned records a successful scan: the controller did its job and the
// result reflects the current spec.
func (s *AtlasSecurityScan) SetScanned(message string) {
	s.setConditions(metav1.ConditionTrue, ReasonScanned, message, false)
	s.Status.Failed = 0
}

// SetIdle restores the ready state when nothing is pending and the resource is
// not stalled, which implies a success exists for the current generation. It
// also clears the failures of a retry whose cause has meanwhile disappeared.
func (s *AtlasSecurityScan) SetIdle() {
	ready := meta.FindStatusCondition(s.Status.Conditions, readyCond)
	msg := "no scan is due"
	if ready != nil && ready.Status == metav1.ConditionTrue {
		msg = ready.Message
	}
	s.setConditions(metav1.ConditionTrue, ReasonScanned, msg, false)
	s.Status.Failed = 0
}

// SetRetrying records a failed attempt that will be retried with a backoff. It
// deliberately leaves Stalled false: a transient failure is not a stall.
func (s *AtlasSecurityScan) SetRetrying(reason, message string) {
	s.setCondition(metav1.Condition{Type: readyCond, Status: metav1.ConditionFalse, Reason: reason, Message: message})
	s.setCondition(metav1.Condition{Type: reconcilingCond, Status: metav1.ConditionTrue, Reason: ReasonRetrying, Message: message})
	s.setCondition(metav1.Condition{Type: stalledCond, Status: metav1.ConditionFalse, Reason: ReasonRetrying})
}

// SetStalled records a failure that retrying cannot fix, or exhausted retries.
func (s *AtlasSecurityScan) SetStalled(reason, message string) {
	s.setCondition(metav1.Condition{Type: readyCond, Status: metav1.ConditionFalse, Reason: reason, Message: message})
	s.setCondition(metav1.Condition{Type: reconcilingCond, Status: metav1.ConditionFalse, Reason: reason, Message: message})
	s.setCondition(metav1.Condition{Type: stalledCond, Status: metav1.ConditionTrue, Reason: reason, Message: message})
	s.Status.ObservedGeneration = s.Generation
}

// SetSuspended pauses the resource. Ready and Compliant keep their last values.
func (s *AtlasSecurityScan) SetSuspended() {
	if meta.FindStatusCondition(s.Status.Conditions, readyCond) == nil {
		s.setCondition(metav1.Condition{Type: readyCond, Status: metav1.ConditionUnknown, Reason: ReasonSuspended, Message: "scanning is suspended"})
	}
	s.setCondition(metav1.Condition{Type: reconcilingCond, Status: metav1.ConditionFalse, Reason: ReasonSuspended, Message: "scanning is suspended"})
	s.setCondition(metav1.Condition{Type: stalledCond, Status: metav1.ConditionFalse, Reason: ReasonSuspended})
	s.Status.ObservedGeneration = s.Generation
}

// SetCompliant records the policy verdict.
func (s *AtlasSecurityScan) SetCompliant(status metav1.ConditionStatus, reason, message string) {
	s.setCondition(metav1.Condition{Type: compliantCond, Status: status, Reason: reason, Message: message})
}

// setConditions writes Ready with the given status, and Reconciling/Stalled to
// match: reconciling when the resource is converging, neither otherwise.
func (s *AtlasSecurityScan) setConditions(ready metav1.ConditionStatus, reason, message string, reconciling bool) {
	s.setCondition(metav1.Condition{Type: readyCond, Status: ready, Reason: reason, Message: message})
	recon := metav1.ConditionFalse
	if reconciling {
		recon = metav1.ConditionTrue
	}
	s.setCondition(metav1.Condition{Type: reconcilingCond, Status: recon, Reason: reason, Message: message})
	s.setCondition(metav1.Condition{Type: stalledCond, Status: metav1.ConditionFalse, Reason: reason})
}

func (s *AtlasSecurityScan) setCondition(cond metav1.Condition) {
	if cond.ObservedGeneration <= 0 {
		cond.ObservedGeneration = s.Generation
	}
	meta.SetStatusCondition(&s.Status.Conditions, cond)
}

// LevelIndex ranks a level, lowest first, and -1 for an unknown one.
func LevelIndex(l SecurityLevel) int {
	for i, k := range SecurityLevels {
		if k == l {
			return i
		}
	}
	return -1
}
