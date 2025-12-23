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

const (
	// ReasonReconciling represents for the reconciliation is in progress.
	ReasonReconciling = "Reconciling"
	// ReasonGettingDevDB represents the reason for getting the dev database.
	ReasonGettingDevDB = "GettingDevDB"
	// ReasonWhoAmI represents the reason for getting the current user via Atlas CLI
	ReasonWhoAmI = "WhoAmI"
	// ReasonApplyingMigration represents the reason for applied a schema/migration resource successfully.
	ReasonApplied = "Applied"
	// ReasonApprovalPending represents the reason for the approval is pending.
	ReasonApprovalPending = "ApprovalPending"
	// ReasonCreatingAtlasClient represents the reason for creating an Atlas client.
	ReasonCreatingAtlasClient = "CreatingAtlasClient"
	// ReasonCreatingWorkingDir represents the reason for creating a working directory.
	ReasonCreatingWorkingDir = "CreatingWorkingDir"
)

// isFailedReason returns true if the given reason is a failed reason.
func isFailedReason(reason string) bool {
	switch reason {
	case ReasonReconciling, ReasonGettingDevDB, ReasonApprovalPending:
		return false
	default:
		return true
	}
}
