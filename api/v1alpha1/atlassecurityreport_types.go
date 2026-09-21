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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type (
	//+kubebuilder:object:root=true
	//
	// AtlasSecurityReportList contains a list of AtlasSecurityReport
	AtlasSecurityReportList struct {
		metav1.TypeMeta `json:",inline"`
		metav1.ListMeta `json:"metadata,omitempty"`

		Items []AtlasSecurityReport `json:"items"`
	}
	//+kubebuilder:object:root=true
	//
	// AtlasSecurityReport holds the findings of the most recent successful scan of an AtlasSecurityScan.
	// It is owned by the scan, replaced on every successful scan, and readable by its own RBAC rather
	// than by everyone who can read the scan.
	// +kubebuilder:printcolumn:name="Findings",type=integer,JSONPath=`.report.summary.total`
	// +kubebuilder:printcolumn:name="Highest",type=string,JSONPath=`.report.summary.highestLevel`
	// +kubebuilder:printcolumn:name="Scanned",type=date,JSONPath=`.report.completionTime`
	// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
	AtlasSecurityReport struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`
		Report            SecurityReport `json:"report,omitempty"`
	}
	// SecurityReport is the graded result of one scan.
	SecurityReport struct {
		StartTime      metav1.Time `json:"startTime"`
		CompletionTime metav1.Time `json:"completionTime"`
		Trigger        ScanTrigger `json:"trigger"`
		// ServerVersion of the database. The engine type is Summary.Driver, which the
		// scan carries too, so it reads without access to the report.
		ServerVersion string       `json:"serverVersion,omitempty"`
		Policy        GradedPolicy `json:"policy"`
		Summary       ScanSummary  `json:"summary"`
		// Extensions installed in the database, by name.
		// +listType=set
		Extensions []string `json:"extensions,omitempty"`
		// +listType=map
		// +listMapKey=id
		// +listMapKey=extension
		Vulnerabilities []ReportedVulnerability `json:"vulnerabilities,omitempty"`
	}
	// GradedPolicy is the policy a report was graded with (waivers appear on the findings they apply to).
	GradedPolicy struct {
		MinSeverity SecurityLevel  `json:"minSeverity"`
		FailOn      *SecurityLevel `json:"failOn,omitempty"`
	}
	// ReportedVulnerability is one finding of a scan.
	ReportedVulnerability struct {
		ID           string        `json:"id"`
		Extension    string        `json:"extension"`
		Version      string        `json:"version,omitempty"`
		Level        SecurityLevel `json:"level"`
		CVSSSeverity string        `json:"cvssSeverity,omitempty"`
		Title        string        `json:"title,omitempty"`
		Description  string        `json:"description,omitempty"`
		Suggestion   string        `json:"suggestion,omitempty"`
		// Waiver is set when the finding is ignored by the policy.
		// +optional
		Waiver *Waiver `json:"waiver,omitempty"`
	}
)

func init() {
	SchemeBuilder.Register(&AtlasSecurityReport{}, &AtlasSecurityReportList{})
}
