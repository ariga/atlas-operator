| `cloud.repo` absent | CEL |
| `devDB` absent | `DevDB` lives on AtlasSchema and AtlasMigration, not on the shared `ProjectConfigSpec`, so a scan has no such field to reject |# Proposal: AtlasSecurityScan, scheduled and change-triggered database security scans

## Background

`atlas security scan` connects to a database, inventories its installed extensions, and reports known vulnerabilities
(the `cve` check) as graded by the Atlas Security Graph. The report carries, per target, the driver, engine version,
the names of installed extensions, and a list of vulnerabilities with an id, the affected extension and its version, a
Security Graph `Level` (`NORMAL`, `ELEVATED`, `HIGH`, `CRITICAL`, always set), a CVSS severity that may be absent, a
title, a description and a suggestion. The command exits non-zero only when a database could not be scanned; findings
alone do not fail it unless `--fail-on` is set. It requires Atlas Pro with the Security Graph enabled. The `cve` check
currently covers PostgreSQL extensions; other databases scan successfully with no findings.

A scan is only useful if it runs again on its own. A database becomes vulnerable without anyone touching the resource that
describes it when a CVE is published against an extension version that is already installed, or when an apply installs a
new extension. The first needs a recurring scan at a time teams can plan around. The second needs a scan as soon as the
change lands. This proposal adds that to the operator.

The Go SDK method `atlasexec.Client.SecurityScan` exists in `ariga.io/atlas` builds newer than v1.3.0 (master pins v1.2.3).
The operator has no admission webhooks; validation is CRD schema plus CEL.

## Goals

- Scan a database on a cron schedule in a named time zone, without drift, catching up a window missed while the operator
  was down with exactly one scan.
- Scan promptly after an AtlasSchema or AtlasMigration that manages the same database completes an apply, coalescing
  bursts and never losing an apply that completes while a scan is running.
- Scan on demand and pause without deleting, without editing GitOps-managed spec.
- Grade findings against a declared policy and expose the verdict separately from controller health.
- A status that Argo CD, Flux/kstatus, `kubectl`, kube-state-metrics and Events can consume without heuristics, and that
  never contains credentials, Secret-derived connection details, or free-text errors that might embed them.
- Fit the existing kinds: `TargetSpec`, `ProjectConfigSpec`, `Cloud`, `Ready`/`Reconciling`/`Stalled`, `observedGeneration`,
  `failed`/`backoffLimit`.

## Non-goals

- Inferring which resources share a database by comparing resolved URLs.
- Changing AtlasSchema or AtlasMigration. The dependency points one way.
- Scan history or trend reports. One report per scan resource, replaced on each successful scan.
- Enforcement. The verdict is observable; nothing blocks an apply.

## Design summary

Two kinds. `AtlasSecurityScan` is the small, widely readable control object: it says when to scan, which resources
change the database, what counts as a violation, and exposes scalars and conditions in status. `AtlasSecurityReport`
is the data object holding the vulnerability list, owned by the scan, replaced on every successful scan, readable by its
own RBAC rather than by everyone who can read the scan.

Change detection compares revisions, not clocks. At the start of a scan the controller reads each trigger's applied
revision (AtlasSchema `status.observed_hash`, AtlasMigration `status.lastAppliedVersion`, plus the object `uid`) and, on
success, records those values in status. A scan is due whenever a live revision differs from the recorded one. An apply
that lands mid-scan changes the live value after the snapshot, so the next reconcile sees a difference and scans again.
A burst of applies changes the live value many times but produces exactly one follow-up scan. No timestamp is ever
compared against the scanner's clock, and a no-op re-apply, which the AtlasSchema controller performs on every Secret
or ConfigMap event, changes no revision and causes no scan.

Every watermark (spec generation, manual request, schedule slot, trigger revisions, active waivers) is snapshotted when a
scan starts and committed to status only when it succeeds, in the same write as the results. A failed scan advances
nothing, so the same trigger is still pending and is retried; a crash or leader failover mid-scan needs no resume logic,
because the next reconcile re-derives what is due from status alone. The requeue timer is always computed from the
current time, never from a status field, so a resource whose scans keep failing still wakes at its next slot.

## API

### Spec

```yaml
apiVersion: db.atlasgo.io/v1alpha1
kind: AtlasSecurityScan
metadata:
  name: app-db
  namespace: payments
  annotations:
    # On-demand scan. Any new value triggers one scan; the value is echoed to
    # status.lastHandledScanRequest when that scan completes successfully.
    db.atlasgo.io/scan-requested-at: "2026-09-09T14:03:11Z"
spec:
  # Target database: TargetSpec, exactly as AtlasSchema/AtlasMigration (url | urlFrom | credentials).
  urlFrom:
    secretKeyRef:
      name: app-db-credentials
      key: url

  # Project configuration: ProjectConfigSpec, exactly as the other kinds (config | configFrom | envName | vars).
  # A custom config may carry `security {}` and `notify {}` blocks; it requires allowCustomConfig=true.
  # It may not decide what is reported: `security.min_severity`, `security.fail_on`, `security.cve.min_severity`,
  # `security.cve.ignore` and `exclude` are rejected for the env that runs, as they drop findings before grading.
  # devDB is not a field here: a scan needs no dev database, so it moves onto the two kinds that do.
  # A config that yields several targets is rejected at scan time.
  envName: kubernetes

  # Atlas Cloud token. Required in practice: the Security Graph is an Atlas Pro feature. `repo` is rejected.
  cloud:
    tokenFrom:
      secretKeyRef:
        name: atlas-cloud
        key: token

  # Trigger 1: wall-clock schedule. 5-field cron or @hourly/@daily/@weekly/@monthly/@yearly, evaluated in timeZone.
  # TZ=/CRON_TZ= prefixes and @every are rejected at admission. Optional, but see "at least one" below.
  schedule: "0 3 * * *"
  timeZone: Europe/Berlin          # IANA name. Default UTC. "Local" is rejected.

  # Trigger 2: resources in this namespace whose applies change this database. Optional.
  # At least one of schedule / triggers must be set.
  triggers:
  - kind: AtlasSchema              # AtlasSchema | AtlasMigration
    name: app-schema
  - kind: AtlasMigration
    name: app-migrations

  # Pause without deleting. Resuming scans once and covers everything that became due meanwhile.
  suspend: false

  # Policy. Levels: NORMAL < ELEVATED < HIGH < CRITICAL (Security Graph grades, as the CLI names them).
  policy:
    minSeverity: ELEVATED          # findings below this level are not reported (--min-severity). Default NORMAL.
    failOn: HIGH                   # a non-waived finding at or above this level makes Compliant=False.
                                   # Unset: Compliant is Unknown/NoThreshold. Must be >= minSeverity.
    ignore:                        # waivers, applied by the operator so waived findings stay in the report
    - id: CVE-2024-10977
      reason: "pgvector not reachable from the app role; SEC-1234"
      expirationTime: "2026-12-31T00:00:00Z"

  backoffLimit: 20                 # retries of a failed scan before Stalled. 0 means unlimited, as in the other kinds.
```

### Status

```yaml
status:
  observedGeneration: 4
  conditions:
  - type: Ready                    # the controller did its job: the last scan succeeded and reflects this spec
    status: "True"
    reason: Scanned
    message: "3 findings (1 CRITICAL, 2 HIGH) in 4 extensions; 1 waived"
    observedGeneration: 4
    lastTransitionTime: "2026-09-10T01:00:12Z"
  - type: Compliant                # policy verdict as of lastSuccessfulTime
    status: "False"
    reason: PolicyViolated
    message: "3 findings at or above HIGH, highest CRITICAL; see atlassecurityreport/app-db"
    observedGeneration: 4
    lastTransitionTime: "2026-09-10T01:00:12Z"
  - type: Reconciling
    status: "False"
    reason: Scanned
    observedGeneration: 4
    lastTransitionTime: "2026-09-10T01:00:12Z"
  - type: Stalled
    status: "False"
    reason: Scanned
    observedGeneration: 4
    lastTransitionTime: "2026-09-10T01:00:12Z"
  lastScan:                        # the most recent attempt, success or failure: "why did it run, what happened"
    trigger: Apply                 # Spec | Manual | Apply | Schedule | Policy
    triggeredBy: "AtlasSchema/app-schema"
    startTime: "2026-09-10T01:00:03Z"
    completionTime: "2026-09-10T01:00:12Z"
    result: Succeeded              # Succeeded | Failed
    inputsHash: "sha256:9b2f..."   # hash of everything observed at start; identifies a distinct set of inputs
  lastSuccessfulTime: "2026-09-10T01:00:12Z"
  lastScheduleTime: "2026-09-10T01:00:00Z"   # latest slot the last successful scan covered (03:00 Berlin)
  nextScheduleTime: "2026-09-11T01:00:00Z"   # the slot after lastScheduleTime; omitted when suspended or unscheduled
  lastHandledScanRequest: "2026-09-09T14:03:11Z"
  triggers:                        # revisions observed when the last successful scan started
  - kind: AtlasSchema
    name: app-schema
    uid: 4c1e0b1d-8f0e-4b1c-9a6b-2c7d0d3f1a10
    revision: "h1:Zm9vYmFy..."     # AtlasSchema.status.observed_hash
  - kind: AtlasMigration
    name: app-migrations
    uid: 9f2a6c7e-1d2b-4e3f-8a9b-0c1d2e3f4a5b
    revision: "20260901120000"     # AtlasMigration.status.lastAppliedVersion
  activeWaivers: [CVE-2024-10977]  # waivers in force when the last successful scan started
  summary:                         # from the last successful scan; never anything URL-derived
    driver: postgres               # from the report, not the URL
    extensions: 4
    total: 3                       # non-waived findings at or above minSeverity
    waived: 1
    highestLevel: CRITICAL         # omitted when total is 0
    levels:                        # always all four, so metric series never disappear
    - level: CRITICAL
      count: 1
    - level: HIGH
      count: 2
    - level: ELEVATED
      count: 0
    - level: NORMAL
      count: 0
  reportRef:
    name: app-db                   # AtlasSecurityReport in the same namespace
  failed: 0                        # consecutive failed attempts since the last success
```

No host, user, database name, query string, CVE id (other than the waiver ids the user wrote in spec), extension name,
or CLI or driver error text appears in status or Events. `summary.driver` is the engine type as the scan reports it.

### Report

```yaml
apiVersion: db.atlasgo.io/v1alpha1
kind: AtlasSecurityReport
metadata:
  name: app-db                     # same name as the scan; replaced on every successful scan
  namespace: payments
  labels:
    db.atlasgo.io/scan: app-db     # plus every label of the scan, so label-scoped operator instances see it
  ownerReferences:
  - apiVersion: db.atlasgo.io/v1alpha1
    kind: AtlasSecurityScan
    name: app-db
    uid: ...
    controller: true
report:
  startTime: "2026-09-10T01:00:03Z"
  completionTime: "2026-09-10T01:00:12Z"
  trigger: Apply
  serverVersion: "16.4"
  policy:                          # the policy this report was graded with
    minSeverity: ELEVATED
    failOn: HIGH
  summary: { ... same shape as the scan's status.summary, and the engine type lives there ... }
  extensions: [pg_partman, pgvector, pg_trgm, hstore]     # names, as the scan reports them
  vulnerabilities:
  - id: CVE-2025-1094
    extension: pg_partman
    version: "5.0.1"               # installed version, reported per vulnerability
    level: CRITICAL                # Security Graph grade; what the policy compares against
    cvssSeverity: CRITICAL         # CVSS rating; may be absent
    title: "..."
    description: "..."             # truncated to 1 KiB
    suggestion: "Upgrade pg_partman to 5.0.2"
  - id: CVE-2024-10977
    extension: pgvector
    version: "0.7.4"
    level: HIGH
    cvssSeverity: MEDIUM
    title: "..."
    waiver:
      reason: "pgvector not reachable from the app role; SEC-1234"
      expirationTime: "2026-12-31T00:00:00Z"
```

The report never contains the target URL in any form or any error text.

### Go types

List types and imports are omitted for brevity; they follow the existing kinds.

```go
package v1alpha1

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

const (
	// AnnotationScanRequestedAt requests a scan. Any new value triggers one scan.
	AnnotationScanRequestedAt = "db.atlasgo.io/scan-requested-at"
	compliantCond             = "Compliant"
)

type (
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
	// +kubebuilder:validation:XValidation:rule="!has(self.failOn) || {'NORMAL':0,'ELEVATED':1,'HIGH':2,'CRITICAL':3}[self.failOn] >= {'NORMAL':0,'ELEVATED':1,'HIGH':2,'CRITICAL':3}[self.minSeverity]",message="failOn must be at or above minSeverity"
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
		ID string `json:"id"`
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
		// LastScheduleTime is the latest schedule slot a successful scan covered. A scan that is still
		// running when a slot passes covers it, so this can be later than the start of that scan.
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
	ScanAttempt struct {
		Trigger        ScanTrigger  `json:"trigger"`
		TriggeredBy    string       `json:"triggeredBy,omitempty"`
		StartTime      metav1.Time  `json:"startTime"`
		CompletionTime *metav1.Time `json:"completionTime,omitempty"`
		Result         ScanResult   `json:"result,omitempty"`
		// Message is a fixed description of a failure class, never CLI or driver output.
		Message string `json:"message,omitempty"`
		// InputsHash identifies what was observed when the attempt started. A stalled
		// resource makes one finished attempt per distinct hash.
		InputsHash string `json:"inputsHash,omitempty"`
	}
	ObservedTrigger struct {
		Kind     ScanTriggerKind `json:"kind"`
		Name     string          `json:"name"`
		UID      types.UID       `json:"uid"`
		Revision string          `json:"revision"`
	}
	ScanSummary struct {
		Driver       string        `json:"driver,omitempty"`
		Extensions   int32         `json:"extensions"`
		Total        int32         `json:"total"`
		Waived       int32         `json:"waived"`
		// +optional
		HighestLevel SecurityLevel `json:"highestLevel,omitempty"`
		// +listType=map
		// +listMapKey=level
		Levels []LevelCount `json:"levels"`
	}
	LevelCount struct {
		Level SecurityLevel `json:"level"`
		Count int32         `json:"count"`
	}

	//+kubebuilder:object:root=true
	//
	// AtlasSecurityReport holds the findings of the most recent successful scan of an AtlasSecurityScan.
	// +kubebuilder:printcolumn:name="Findings",type=integer,JSONPath=`.report.summary.total`
	// +kubebuilder:printcolumn:name="Highest",type=string,JSONPath=`.report.summary.highestLevel`
	// +kubebuilder:printcolumn:name="Scanned",type=date,JSONPath=`.report.completionTime`
	// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
	AtlasSecurityReport struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`
		Report            SecurityReport `json:"report,omitempty"`
	}
	SecurityReport struct {
		StartTime      metav1.Time  `json:"startTime"`
		CompletionTime metav1.Time  `json:"completionTime"`
		Trigger        ScanTrigger  `json:"trigger"`
		ServerVersion  string       `json:"serverVersion,omitempty"`
		Policy         GradedPolicy `json:"policy"`
		Summary        ScanSummary  `json:"summary"`
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
```

Condition reasons added to `reason.go`: `Scanning`, `Scanned`, `Retrying`, `ScanFailed`, `LoginFailed`, `CLIError`,
`ReadingInputs`, `StoringReport`, `BackoffLimitExceeded`, `InvalidSchedule`, `InvalidTimeZone`, `InvalidTarget`,
`Suspended`, `NotScanned`, `NoThreshold`, `WithinPolicy`, `PolicyViolated`, `ReportStale`. Event-only reasons:
`TriggerNotFound`, `MissedSchedule`, `ScanWarning`, `Resumed`.

Validation at admission, all schema or CEL:

| Rule | Mechanism |
|---|---|
| at least one of `schedule` / `triggers` | CEL on spec |
| no `TZ=`/`CRON_TZ=` prefix, no `@every` | CEL on spec |
| `timeZone` not `Local` | CEL on spec |
| `kind` is AtlasSchema or AtlasMigration; (kind, name) unique | Enum + `listType=map` |
| levels are NORMAL/ELEVATED/HIGH/CRITICAL; `failOn >= minSeverity` | Enum + CEL map literal |
| ignore ids well-formed and unique; reason required | Pattern + `listType=map` + MinLength |
| `cloud.repo` absent | CEL |
| `devDB` absent | `DevDB` moves onto AtlasSchema and AtlasMigration, so the shared `ProjectConfigSpec` a scan inlines has no such field and the schema prunes it |
| a target or a project config is present; exactly one target results | controller: `Stalled=True/InvalidTarget` (`urlFrom` and `credentials` are non-pointer structs that Go clients always serialize, so a CEL `has()` on them would be trivially true) |
| cron parses and fires within the parser's horizon; IANA zone exists | controller: `Stalled=True` with `InvalidSchedule`, `InvalidTimeZone` |

`timeZone` defaults to UTC without any rule tying it to `schedule`, so a change-triggered-only resource is admitted
after defaulting.

## Semantics

### Scheduling

The schedule is parsed with `robfig/cron/v3` using the standard parser (minute, hour, day-of-month, month, day-of-week,
plus the fixed descriptors). `@every` is rejected because it is anchored at parse time and drifts. An expression that
parses but does not fire within the library's five-year search horizon (for example `0 3 30 2 *`) makes `Next` return
the zero time; the controller checks `Next(now).IsZero()` after parsing and stalls with `InvalidSchedule`, and every
helper treats a zero `Next` as "no slot". The time zone is resolved with `time.LoadLocation`; the binary imports
`time/tzdata`, since the runtime image is Alpine without the tzdata package. DST follows the library: a wall-clock time
that does not exist on a spring-forward day is skipped; on a fall-back day the repeated hour produces two slots one hour
apart and therefore two scans.

The anchor is `status.lastScheduleTime`. Before the first successful scan under a schedule, the anchor is the later of
`metadata.creationTimestamp` and `status.lastSuccessfulTime`, so a resource that existed for a year before a schedule
was added does not report a year of missed slots; when the schedule is removed, `lastScheduleTime` is cleared on the
next successful scan. A scheduled scan is due when `Next(anchor) <= now`. When a scan succeeds, `lastScheduleTime`
becomes the latest slot at or before its completion, so a slot it was still running at is covered by it rather than
queued behind it, and `nextScheduleTime` becomes `Next(lastScheduleTime)` and so never sits in the past.
Consequences:

- Ticks come from the cron expression in the zone, never from "last run plus interval", so nothing drifts.
- A window missed while the operator was down is caught up with exactly one scan; `lastScheduleTime` jumps to the latest
  missed slot and intermediate slots are skipped. The loop that finds the latest slot is capped at 100 000 steps, after
  which it anchors at `now`, as the CronJob controller does. A Normal event `MissedSchedule` records the count skipped;
  it is emitted only when the scan was not caused by a spec change or a resume, so a schedule edit or a pause does not
  report its gap as missed.
- A scan for any trigger covers every slot at or before its completion, so a slot that passes while a scan is running is
  covered by it and no follow-up is queued for it, and a schedule finer than the scan takes does not scan back to back.
  A scheduled scan never runs early.
- An attempt that fails or stalls commits no watermark, so a slot that passed while it ran is still due and the resource
  is requeued at once. `MissedSchedule` counts to the slot observed when the scan started, not to the one it covered:
  the slots it spanned are covered, not missed.
- `nextScheduleTime` only advances on success, so it stays at a missed slot until the catch-up completes. That is what
  makes an "overdue" alert expressible. It is a status field for readers, not the controller's timer.
- The requeue timer is `Next(now)` (and the earliest future waiver expiry, and the retry delay when retrying), computed
  fresh at the end of every reconcile and floored at one second. It never reads `nextScheduleTime`, which can be in the
  past while a slot is uncommitted; a `RequeueAfter` at or below zero would not requeue at all.
- A fresh resource is scanned immediately with trigger `Spec`; that scan sets `lastScheduleTime` to the latest slot it
  covered, so a window that predates creation is never caught up and the first scheduled scan is the next real slot.

The proposal borrows CronJob's model (declarative schedule, durable anchor in status, wake-up as a hint) and differs in
three stated ways: it always catches up with exactly one scan and so needs no `startingDeadlineSeconds`; it needs no
`concurrencyPolicy` because reconciles for one resource are serialized; and it defaults the time zone to UTC rather than
to the controller's local zone.

### Change triggering

For each entry in `spec.triggers` the controller reads the object and takes its revision: `status.observed_hash` for
AtlasSchema, `status.lastAppliedVersion` for AtlasMigration, together with `metadata.uid`. A trigger is pending when the
object exists and either no revision is recorded for it, its uid differs from the recorded one (deleted and recreated),
or its revision differs. A pending trigger makes the scan due with trigger `Apply` and `triggeredBy` naming the objects.

Why these tokens. `observed_hash` is written only when the AtlasSchema controller reaches its ready state, from the hash
of the desired schema; it is never written on a failed apply and is unchanged by the no-op re-applies that the controller
performs on every event of a referenced Secret or ConfigMap (those bump `last_applied` only). It can change without a
database change, when a new desired state plans to no changes, which costs one harmless scan. `lastAppliedVersion` is
written on successful `migrate apply` and `migrate down` and when the directory is already applied, never on failure.
Three limits are accepted and documented: `observed_hash` is the hash of the desired schema, so an apply that repairs
manual drift without a desired-state change is not detected; a `migrate apply` that re-runs an edited version under
`non-linear` execution order can change the database without changing `lastAppliedVersion`; and an apply that fails
part-way under `txMode: none` can install an extension while neither token moves. All three are covered by the
schedule. A `status.changeSequence` counter on the two kinds, incremented only when an apply modified the database,
would close them; it is listed under open questions rather than required, because the tokens that exist today are
sufficient for the stated requirements.

A trigger that does not exist, or is not visible to this operator instance under label-selector scoping, is a
configuration mistake, not a scan failure: a Warning event `TriggerNotFound` is recorded, no entry is written for it, and
the scan proceeds. Once the object exists and applies, its revision differs from "unrecorded" and the scan runs.

### On demand

Setting or changing the annotation `db.atlasgo.io/scan-requested-at` makes the scan due with trigger `Manual` whenever
the value differs from `status.lastHandledScanRequest`. The value is echoed to status only when the scan that consumed
it completes successfully, so `kubectl wait --for=jsonpath='{.status.lastHandledScanRequest}'=<value>` returns when the
result exists, not when the request was noticed. If that scan keeps failing, the wait times out and `Ready` says why.
GitOps-managed scans need the annotation excluded from diffs; see the Argo CD section.

```sh
T=$(date -u +%FT%TZ)
kubectl annotate atlassecurityscan/app-db db.atlasgo.io/scan-requested-at="$T" --overwrite
kubectl wait atlassecurityscan/app-db --for=jsonpath="{.status.lastHandledScanRequest}=$T" --timeout=10m
```

### Suspend

`spec.suspend` is checked before anything else. While it is true no scan runs and no validation is performed:
`Reconciling` and `Stalled` are False with reason `Suspended`, `Ready` and `Compliant` keep their last values,
`nextScheduleTime` is omitted so an overdue alert cannot fire, and `observedGeneration` is stamped so kstatus does not
report the object as in progress. Setting it back to false is a spec change and produces one scan with trigger `Spec`;
that scan covers every slot, apply, request and waiver expiry that became due while paused, because all watermarks
advance from its start snapshot. A schedule that became invalid while suspended surfaces as `Stalled` on resume.

### Policy

`minSeverity` is passed to the CLI as `--min-severity`, so findings below it never appear in the report or the counts.
It is always passed, defaulting to `NORMAL`, so the severity the report is graded with is the one the scan ran with.
`--fail-on` is not passed: the operator grades the report itself, which gives one source of truth for the verdict and
avoids the ambiguity of an exit code that means both "threshold reached" and "target not scanned". Waivers are applied
by the operator rather than through `--ignore`, so a waived finding stays in the report marked with its reason and
expiry; it does not count toward `summary.total` or the verdict.

For the same reason a custom `atlas.hcl` may not carry the policy. The CLI appends configured ignores to the ones it is
given, and treats a configured `cve.min_severity` as a floor that `--min-severity` cannot lower, so either would drop a
finding before the operator ever saw it while `report.policy` still described the policy of the CR. A configuration that
sets `security.min_severity`, `security.fail_on`, `security.cve.min_severity` or `security.cve.ignore` for the env that
runs, or in the top-level block that env extends, is therefore rejected as a permanent input error. So is `exclude` on
that env: it reaches the realm inspection, so an excluded extension never reaches the Security Graph and the scan
reports nothing while still succeeding. Everything else, `notify` included, is merged unchanged, and another env's
`security` block is left alone.

`security.cve.timeout` is allowed: a Security Graph that does not answer in time fails the target, which the operator
records as `ScanFailed` and retries, so it cannot produce a clean verdict.

Waiver expiry is handled like every other trigger, by comparing recorded state rather than clocks: the set of waivers in
force at scan start (no expiry, or expiry after the start) is recorded on success as `status.activeWaivers`, and a scan
is due with trigger `Policy` whenever the set in force now differs from the recorded one. Adding or removing a waiver is
a spec change and is covered by `Spec`. The requeue timer includes the earliest future expiry among the waivers in force,
so the verdict is recomputed at the minute a waiver lapses. If the database is unreachable at that moment, the verdict
stands on the expired waiver until a scan succeeds; `Ready=False` and, eventually, the Stalled alert make that visible.

`Compliant` is evaluated on the non-waived findings of the last successful scan: `False/PolicyViolated` if any is at or
above `failOn`, `True/WithinPolicy` otherwise, `Unknown/NoThreshold` when `failOn` is unset, `Unknown/NotScanned` before
the first successful scan, and `Unknown/ReportStale` once the resource has stalled on `BackoffLimitExceeded`, because a
verdict about a database that could not be scanned for that long should not read as a pass. A transient failure that is
still being retried leaves the verdict in place.

### Failures and retries

`failed` counts consecutive failed attempts since the last success and is reset only by a success. While `backoffLimit`
is 0 or `failed <= backoffLimit`, a failed attempt is retried after `backoffDelayAt(failed)` (the existing linear
backoff) with `Ready=False` under a failure reason and `Reconciling=True/Retrying`. When `failed` exceeds
`backoffLimit`, the resource stalls: `Stalled=True/BackoffLimitExceeded`, `Reconciling=False`,
`Compliant=Unknown/ReportStale`, `observedGeneration` stamped, and no more backoff retries. A stalled resource makes
exactly one attempt for each distinct set of inputs that becomes due afterwards (the next schedule slot, a changed
trigger revision, a new request value, a new generation, a lapsed waiver), identified by `lastScan.inputsHash`. An
attempt spends its input set by finishing, not by starting, so one interrupted between the two status writes is retried
rather than counted; the
conditions do not change during or after a failed attempt, so a database that is down for a week reads as Stalled once
rather than flapping between Retrying and Stalled at every slot. The first success clears `failed` and every failure
condition.

Failure reasons are a closed set and their messages are fixed text ("the database could not be scanned; attempt 3 of
20; see the operator log"). CLI and driver output is never copied into a condition, an Event or `lastScan.message`:
connection errors contain the resolved address of a Secret-held hostname, and drivers reformat user, host and database
in ways a string replacement cannot catch. The full error goes to the operator log.

## Conditions

`Ready` answers "is the controller done and does the result reflect this spec". `Compliant` answers "is the database
within the declared policy as of the last successful scan". `Reconciling` and `Stalled` follow kstatus. `Reconciling`
means the controller is converging on a new spec or retrying a failure; it is set while the first scan and any
`Spec`-triggered scan runs and while a failed scan is being retried. A scheduled, apply-, manual- or policy-triggered
re-scan that succeeds changes no condition status (the `Ready` message's counts may change): it is visible while running
through `lastScan.startTime` without a `completionTime`, and afterwards through the updated summary. Health in Argo CD
and Flux therefore moves only on spec changes and failures, never on a routine successful scan. Every condition carries
`observedGeneration`.

| State | Ready | Reconciling | Stalled | Compliant |
|---|---|---|---|---|
| First visit | Unknown / Reconciling | True / Reconciling | False / Reconciling | Unknown / NotScanned |
| First scan running | Unknown / Scanning | True / Scanning | False / Scanning | Unknown / NotScanned |
| `Spec`-triggered re-scan running | unchanged | True / Scanning | False / Scanning | unchanged |
| Other re-scan running | unchanged | unchanged | unchanged | unchanged |
| Succeeded, `failOn` unset | True / Scanned | False / Scanned | False / Scanned | Unknown / NoThreshold |
| Succeeded, within policy | True / Scanned | False / Scanned | False / Scanned | True / WithinPolicy |
| Succeeded, violated | True / Scanned | False / Scanned | False / Scanned | False / PolicyViolated |
| Failed, retrying | False / ScanFailed, LoginFailed, CLIError, ReadingInputs, StoringReport | True / Retrying | False / Retrying | unchanged |
| Pending trigger disappeared while retrying (request withdrawn, trigger deleted) | True / Scanned | False / Scanned | False / Scanned | unchanged; `failed` reset |
| Backoff limit exceeded, including its one-attempt-per-input retries | False / BackoffLimitExceeded | False / BackoffLimitExceeded | True / BackoffLimitExceeded | Unknown / ReportStale |
| Invalid schedule, time zone or target | False / InvalidSchedule, InvalidTimeZone, InvalidTarget | False / same | True / same | unchanged |
| Suspended | unchanged (Unknown / Suspended if never scanned) | False / Suspended | False / Suspended | unchanged |

Two deliberate deviations from the sibling helpers. A transient failure sets `Reconciling=True/Retrying` rather than
`Stalled=True`, so a 30-second blip does not read as Failed in Flux or Degraded in Argo; `Stalled` is reserved for
exhausted retries and permanent input errors. And `observedGeneration` is committed only on success, on `Stalled`
transitions, and on suspend; a spec edit followed by failing scans therefore reads as in progress until the retries are
exhausted, at which point it reads as Failed with the generation observed. Both need condition setters specific to this
kind rather than a copy of `SetNotReady`.

Condition messages carry counts, levels, times and the report name only. Never a CVE id, extension name, or anything
derived from the connection URL or from an error.

Tooling consequences:

- kstatus and Flux: generation mismatch or `Reconciling=True` is InProgress, `Stalled=True` is Failed, otherwise
  `Ready=True` is Current. A policy violation is Current, so a Flux `Kustomization` with health checks does not hang on a
  CVE a re-sync cannot fix.
- `kubectl wait --for=condition=Ready` returns when a result reflecting the current spec exists;
  `--for=condition=Compliant` is a CI gate that, by design, never returns when `failOn` is unset;
  `--for=jsonpath='{.status.summary.total}'=0` waits for a clean report.

## Behaviour on change

| Change | Effect |
|---|---|
| Resource created | first visit writes conditions; next reconcile scans (`Spec`); `lastScheduleTime` set to the latest slot that scan covered |
| `schedule` or `timeZone` edited | generation bump: validated; invalid or never-firing → `Stalled/InvalidSchedule` or `InvalidTimeZone`, no scan; valid → scan now (`Spec`), slots recomputed under the new schedule, no `MissedSchedule` event |
| `triggers` edited | scan now (`Spec`); removed entries drop out of `status.triggers`, added ones are recorded by that scan |
| `policy` edited | scan now (`Spec`); report and verdict recomputed with the new policy |
| `suspend: true` | no scans, no validation; `Reconciling=False/Suspended`; `nextScheduleTime` omitted; `Ready` and `Compliant` retained |
| `suspend: false` | scan now (`Spec`), covering everything pending; `Resumed` event |
| annotation `db.atlasgo.io/scan-requested-at` set or changed | scan now (`Manual`); echoed to `lastHandledScanRequest` on success |
| a trigger's AtlasSchema applies a new desired schema (`observed_hash` changes) | scan promptly (`Apply`); no condition changes |
| a trigger's AtlasSchema re-applies with no change (Secret or ConfigMap event; `last_applied` bumps) | nothing |
| a trigger's AtlasMigration applies or migrates down (`lastAppliedVersion` changes) | scan promptly (`Apply`) |
| an apply completes while a scan is running | one more scan right after the first returns |
| a burst of applies during a scan | exactly one follow-up scan |
| a trigger is missing or mistyped | Warning `TriggerNotFound`; scan proceeds; no entry recorded |
| a trigger is deleted and recreated | uid differs; scan after its first apply |
| a scheduled scan completes | summary, report and timestamps updated; condition statuses unchanged unless the verdict changed |
| operator down across one or many slots | one catch-up scan on start; `MissedSchedule` event with the count |
| operator crash or leader failover mid-scan | nothing was committed; the next reconcile finds the same trigger pending and scans; an attempt left open whose trigger has meanwhile disappeared is closed as `Failed` with message "interrupted" |
| database unreachable | `Ready=False/ScanFailed`, `Reconciling=True/Retrying`, backoff retries up to `backoffLimit`, then `Stalled/BackoffLimitExceeded` and `Compliant=Unknown/ReportStale`; from then on one attempt per new input set (next slot, changed trigger, new request, spec change, lapsed waiver); the first success clears everything |
| Atlas token invalid or not Pro | same path; reason `LoginFailed` |
| a referenced Secret rotated | nothing until the next scan, which reads the new value. Secrets are not watched by this controller |
| a waiver's `expirationTime` passes | scan (`Policy`); verdict recomputed |
| a slot passes while a scan is running | the scan covers it: `lastScheduleTime` moves to that slot, and `nextScheduleTime` stays ahead of the completion |
| a waiver expires while a scan is running | the findings were graded with it, so the resource is requeued at once and the next pass regrades (`Policy`) |
| a custom `atlas.hcl` sets a policy attribute | rejected before the CLI runs: attempt closed as `Failed`; `Stalled/InvalidTarget` naming the attribute |
| a custom `atlas.hcl` yields several targets | attempt closed as `Failed`; `Stalled/InvalidTarget`; one database per resource |
| `AtlasSecurityReport` deleted by a user | recreated by the next scan; `reportRef` dangles until then |
| `AtlasSecurityScan` deleted | report garbage-collected through the owner reference; no finalizer |

## Reconciliation

One controller, at most one reconcile per object at a time (workqueue guarantee), scans run in-process through
`atlasexec.Client.SecurityScan` like `schema apply` does today.

```
Reconcile(req):
  res := Get(req); if not found: return                    # report is garbage-collected through its ownerReference
  defer: write status with RetryOnConflict, copying res.Status onto the latest object (existing pattern);
         a write error is RETURNED so the workqueue retries (the siblings only log it)

  if len(res.Status.Conditions) == 0:                        # first visit
      SetFirstVisit(res); return Requeue

  # ---- suspend, before anything else ----
  if spec.suspend:
      SetSuspended(res); res.Status.NextScheduleTime = nil; res.Status.ObservedGeneration = generation
      closeOpenAttempt(res, "suspended")
      return                                                # no timer; resume is a spec change

  # ---- validation that no scan can fix: stamp the generation, stall, no timer ----
  now := clock.Now()
  loc, err   := time.LoadLocation(spec.timeZone or "UTC")               → Stalled InvalidTimeZone
  sched, err := parser.Parse(spec.schedule) if set                       → Stalled InvalidSchedule
  if sched != nil && sched.Next(now).IsZero()                            → Stalled InvalidSchedule ("does not fire")
  if no url/urlFrom/credentials and no config                            → Stalled InvalidTarget
  if custom config present and !allowCustomConfig                        → Stalled InvalidTarget

  # ---- observe everything that can make a scan due; this is the start snapshot ----
  snap := {
    generation:  generation,
    requestedAt: annotations[scan-requested-at],
    triggers:    for each spec.triggers: Get; if found {kind, name, uid, revision};
                 if NotFound Warning TriggerNotFound; any other error is returned so the workqueue retries,
    slot:        latestSlotAtOrBefore(sched, anchor(res), now, loc) or nil,   # zero Next means "no slot"
    waivers:     ids of policy.ignore with no expirationTime or expirationTime > now,
    start:       now,
  }
  snap.hash := hash(snap.generation, snap.requestedAt, snap.triggers, snap.slot, snap.waivers)

  # ---- due check: reads spec, status and the snapshot; nothing in memory survives a restart ----
  due, by := nil
  if generation != status.observedGeneration:                            due=Spec,     by="generation N"
  else if snap.requestedAt != "" && snap.requestedAt != status.lastHandledScanRequest:
                                                                           due=Manual,   by=snap.requestedAt
  else if pending := pendingTriggers(snap.triggers, status.triggers); len(pending) > 0:
                                                                           due=Apply,    by=join(pending)
  else if snap.slot != nil && (status.lastScheduleTime == nil || snap.slot > status.lastScheduleTime):
                                                                           due=Schedule, by=snap.slot
  else if set(snap.waivers) != set(status.activeWaivers):                  due=Policy,   by="waivers changed"
  if due == nil:
      closeOpenAttempt(res, "interrupted")                                # crash between START and END, trigger gone
      if !stalled(res): SetIdle(res)                                      # Ready=True/Scanned, Reconciling=False, failed=0:
                                                                          # nothing pending implies a success for this generation exists
      return RequeueAfter(wake(sched, loc, now, snap.waivers, policy))

  # ---- stalled: one FINISHED attempt per distinct set of inputs ----
  if stalled(res, BackoffLimitExceeded) && status.lastScan != nil && status.lastScan.completionTime != nil
     && snap.hash == status.lastScan.inputsHash:
      return RequeueAfter(wake(...))

  # ---- START: record the attempt; commit no watermark ----
  if Reconciling.reason == Suspended: Event Normal Resumed
  res.Status.LastScan = {due, by, start: now, inputsHash: snap.hash}
  if !stalled(res) && (status.lastSuccessfulTime == nil || due == Spec):
      SetScanning(res)                     # first scan: Ready=Unknown/Scanning; later Spec scans: Ready unchanged
  write status now

  # ---- run ----
  data, err := extractData(res)                                          # URL, config, vars, token → failure ReadingInputs
                                                                         # a custom config that sets the policy → Stalled InvalidTarget
  wd   := atlasexec.NewWorkingDir(WithAtlasHCL(data.render))             # env "kubernetes" { url = ... } merged with custom config
  cli  := r.atlasClient(wd.Path(), data.Cloud, ns/name)
  if token: cli.Login(GrantOnly)                                         # → failure LoginFailed
  ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
  scan, err := cli.SecurityScan(ctx, &SecurityScanParams{Env, Vars, MinSeverity: policy.minSeverity})

  # ---- classify ----
  switch:
    scan == nil:                                                          → failure CLIError
    len(scan.Targets) == 0:                                               → failure ScanFailed
    len(scan.Targets) > 1:                                                closeAttempt(Failed, "N targets; one database per resource")
                                                                          SetStalled(InvalidTarget); ObservedGeneration = generation
                                                                          return RequeueAfter(wake(...))
    len(scan.Failures()) > 0 || scan.Targets[0].Error != "":              → failure ScanFailed
    err != nil:                                                           Event Warning ScanWarning (fixed text); continue
  on failure(reason):
      failed++; closeAttempt(Failed, fixedText(reason, failed, backoffLimit))
      log the full error at Error level (the only place it appears)
      if stalled(res, BackoffLimitExceeded): return RequeueAfter(wake(...))   # conditions unchanged once retries are exhausted
      if backoffLimit > 0 && failed > backoffLimit:
          SetBackoffExceeded(res); ObservedGeneration = generation; SetCompliant(Unknown, ReportStale)
          Event Warning BackoffLimitExceeded; return RequeueAfter(wake(...))
      SetRetrying(res, reason); Event Warning reason
      return RequeueAfter(min(backoffDelayAt(failed), wake(...)))

  # ---- grade ----
  t := scan.Targets[0]
  findings := t.Vulnerabilities; attach Waiver to findings whose id is in snap.waivers
  exts     := sorted unique t.Extensions                                # summary.extensions is len(exts), the list the report stores
  summary  := summarize(findings, t.Driver, len(exts))
  verdict  := NoThreshold if failOn unset; PolicyViolated if any unwaived f.Level >= failOn; else WithinPolicy

  # ---- publish the report first, so reportRef never dangles ----
  report := build(t, findings, summary, snap, now); labels copied from res; ownerReferences = [controller ref to res]
  Get via the uncached API reader; Create or Update by name res.Name             # → failure StoringReport
  (the cached client is label-scoped and would miss a pre-existing report that lacks this instance's labels)

  # ---- commit every watermark from the snapshot, with the results, in one write ----
  st := &res.Status
  st.ObservedGeneration = snap.generation; st.LastHandledScanRequest = snap.requestedAt
  st.Triggers = snap.triggers; st.ActiveWaivers = snap.waivers
  st.LastScheduleTime = snap.slot                                        # nil clears it when the schedule was removed
  st.NextScheduleTime = sched != nil && !sched.Next(anchor(res)).IsZero() ? sched.Next(anchor(res)) : nil
  closeAttempt(Succeeded); st.LastSuccessfulTime = now
  st.Summary = summary; st.ReportRef = {res.Name}; st.Failed = 0
  SetScanned(res, summary); SetCompliant(res, verdict)
  Event Normal Scanned; if violated: Event Warning PolicyViolated
  if due != Spec && previous LastScheduleTime != nil && slots skipped between it and snap.slot > 0: Event Normal MissedSchedule
  if activeWaivers(res, completion) != snap.waivers: return RequeueAfter(1s)   # a waiver lapsed while the scan ran
  return RequeueAfter(wake(...))
```

Definitions:

- `pendingTriggers`: for each observed trigger, pending if no entry with that kind and name is recorded, or the recorded
  uid differs, or the recorded revision differs. Inequality only. Triggers that were not found are not pending, which is
  why only `NotFound` may omit one: any other error has to reach the workqueue, or the single enqueue the revision
  change produced is spent on a pass that finds nothing pending, and a trigger-only scan never runs.
- `anchor`: `status.lastScheduleTime` if set, else the later of `metadata.creationTimestamp` and `status.lastSuccessfulTime`.
- `latestSlotAtOrBefore`: steps `Next` from the anchor while the result is non-zero and at or before `now`; returns the
  last such value, or nil if there is none. Capped at 100 000 steps, then `now`.
- A slot the scan was still running at is committed as `lastScheduleTime`, not the slot the scan started from: the scan
  observed the database across that instant, so it covers it. This keeps `nextScheduleTime` ahead of the completion and
  keeps a schedule finer than a scan paced by the schedule instead of scanning back to back.
- A waiver that lapses while the scan runs is the one case that needs another scan, since the findings were graded as
  waived with it; the resource is requeued at once. The other triggers need no catch: a watch wakes it for each of them.
- `fail` and `stall` commit no watermark, so they measure `wake` from the snapshot rather than from the end of the
  attempt. A slot or an expiry that passed while the attempt ran is then still ahead of the baseline and yields an
  immediate requeue, instead of being skipped for a whole period.
- `wake`: the earliest of `Next(now)` (when scheduled and non-zero) and the earliest future `expirationTime` among the
  waivers in force; floored at one second. When retrying, the retry delay is taken if earlier. Computed from `now` on
  every path that sets a timer, including stalled and failed ones, so a scheduled resource never goes dead; the initial
  list on start or failover re-arms it for every object. Permanent validation stalls and suspension set no timer, since
  only a spec change can end them. When nothing is scheduled and no waiver expires, no timer is set and the resource is
  event-driven.
- `inputsHash`: identifies the set of inputs an attempt observed. It does not reset `failed`; it gates a stalled
  resource to one finished attempt per distinct input set, whatever label the precedence rule assigns to it. The gate
  reads `completionTime` as well, so an attempt lost to a crash or a failed status write does not spend its inputs.
- Precedence `Spec > Manual > Apply > Schedule > Policy` decides only the label in `lastScan.trigger`. One scan satisfies
  everything pending, because every watermark advances from the same snapshot.
- `closeOpenAttempt`: if `lastScan` has a `startTime` and no `completionTime`, set `result: Failed` with the given
  fixed message, so a crash never leaves an attempt open forever.
- `SetIdle`: reached only when nothing is due and the resource is not stalled, which implies a success exists for the
  current generation (otherwise `Spec` would be due); it restores `Ready=True/Scanned`, clears `Reconciling`, and resets
  `failed`, so a request withdrawn or a trigger deleted during retries does not leave the resource in a failed state.

### Fan-out

```go
const triggerIndex = ".spec.triggers" // key "<Kind>/<name>"

func (r *AtlasSecurityScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dbv1alpha1.AtlasSecurityScan{}, triggerIndex,
		func(o client.Object) []string {
			s := o.(*dbv1alpha1.AtlasSecurityScan)
			keys := make([]string, 0, len(s.Spec.Triggers))
			for _, t := range s.Spec.Triggers {
				keys = append(keys, string(t.Kind)+"/"+t.Name)
			}
			return keys
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: runtime.NumCPU()}).
		For(&dbv1alpha1.AtlasSecurityScan{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},  // spec edits
			predicate.AnnotationChangedPredicate{},  // scan-requested-at (other annotation edits cost one idle reconcile)
		))).
		Watches(&dbv1alpha1.AtlasSchema{},
			handler.EnqueueRequestsFromMapFunc(r.scansTriggeredBy("AtlasSchema")),
			builder.WithPredicates(revisionChanged(func(o client.Object) string {
				return o.(*dbv1alpha1.AtlasSchema).Status.ObservedHash
			}))).
		Watches(&dbv1alpha1.AtlasMigration{},
			handler.EnqueueRequestsFromMapFunc(r.scansTriggeredBy("AtlasMigration")),
			builder.WithPredicates(revisionChanged(func(o client.Object) string {
				return o.(*dbv1alpha1.AtlasMigration).Status.LastAppliedVersion
			}))).
		Complete(r)
}

// revisionChanged passes an Update whose applied revision moved, and a Create that
// already carries one: a recreated or newly matching resource arrives as an Add.
func revisionChanged(rev func(client.Object) string) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return rev(e.Object) != "" },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return rev(e.ObjectOld) != rev(e.ObjectNew) },
	}
}

func (r *AtlasSecurityScanReconciler) scansTriggeredBy(kind string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list dbv1alpha1.AtlasSecurityScanList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{triggerIndex: kind + "/" + o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	}
}
```

- The predicate is the same comparison the due-check makes, so an event always corresponds to a real pending trigger.
  An event that is lost costs nothing: the due-check reads live revisions on the next reconcile, and the schedule
  guarantees one.
- The watched status updates arrive on the informers the manager already runs for the two kinds. No new list/watch RBAC.
- No `Owns(&AtlasSecurityReport{})`: a deleted report is recreated by the next scan, and recreating it earlier would
  require a scan anyway. No Secret or ConfigMap watches: a credential rotation must not cause a scan, and the controller
  behaves identically under `WATCH_SECRETS=true` and `false` (Secret reads go through the client either way).
- Cache scoping: both kinds are added to `cacheOptions.ByObject` with the label selector in `cmd/main.go`, and the report
  carries the scan's labels, so a label-scoped instance sees its own owned objects. The report is read through the
  uncached API reader before it is written, so a report left by another instance or an older version is updated rather
  than colliding. A trigger must be managed by the same instance; otherwise it is invisible to that instance's cache and
  reported as `TriggerNotFound`. Documented in `values.yaml`.

## Observability

### kubectl

```
$ kubectl get atlassecurityscans
NAME     READY   REASON    COMPLIANT   FINDINGS   HIGHEST    LAST SCAN   NEXT SCAN              AGE
app-db   True    Scanned   False       3          CRITICAL   8h          2026-09-11T01:00:00Z   30d

$ kubectl get atlassecurityreports
NAME     FINDINGS   HIGHEST    SCANNED   AGE
app-db   3          CRITICAL   8h        30d
```

`Next Scan` is a string column because a `date` column renders a future timestamp as `<invalid>`. `Age` is declared
explicitly because the default column disappears once printer columns are set.

### Argo CD

Argo CD computes no health for a custom resource without a check, so this must ship. Contribute
`resource_customizations/db.atlasgo.io/AtlasSecurityScan/health.lua` with `health_test.yaml` fixtures for every row of
the condition table, and publish the same script for `argocd-cm` under
`resource.customizations.health.db.atlasgo.io_AtlasSecurityScan` for older installations.

A policy violation is Healthy with an "out of policy" message by default. Degraded fails a multi-wave sync and stops
auto-sync from retrying that revision, so a CVE published overnight would block unrelated deploys in every Application
containing the scan; the security signal belongs to the alert below, not to sync health. Teams that want the red flag
flip one line.

```lua
-- Health for db.atlasgo.io/AtlasSecurityScan. Status follows kstatus conventions
-- (Ready, Reconciling, Stalled, observedGeneration) plus a policy condition "Compliant".
local degradeOnPolicyViolation = false

local hs = { status = "Progressing", message = "Waiting for the first security scan" }

if obj.spec ~= nil and obj.spec.suspend == true then
  hs.status = "Suspended"
  hs.message = "Security scanning is suspended"
  return hs
end
if obj.status == nil or obj.status.conditions == nil then
  return hs
end
if obj.metadata.generation ~= nil and obj.status.observedGeneration ~= nil
   and obj.status.observedGeneration < obj.metadata.generation then
  hs.message = "Waiting for the Atlas Operator to observe generation " .. tostring(obj.metadata.generation)
  return hs
end

local ready, compliant, reconciling, stalled
for _, c in ipairs(obj.status.conditions) do
  if c.type == "Ready" then ready = c
  elseif c.type == "Compliant" then compliant = c
  elseif c.type == "Reconciling" then reconciling = c
  elseif c.type == "Stalled" then stalled = c end
end

if stalled ~= nil and stalled.status == "True" then
  hs.status = "Degraded"
  hs.message = (stalled.reason or "Stalled") .. ": " .. (stalled.message or "")
  return hs
end
if reconciling ~= nil and reconciling.status == "True" then
  hs.status = "Progressing"
  hs.message = reconciling.message or "Scanning"
  return hs
end
if ready ~= nil and ready.status == "True" then
  hs.status = "Healthy"
  hs.message = ready.message or "Scanned"
  if compliant ~= nil and compliant.status == "False" then
    hs.message = "Out of policy: " .. (compliant.message or "")
    if degradeOnPolicyViolation then hs.status = "Degraded" end
  end
  return hs
end
hs.status = "Progressing"
if ready ~= nil then hs.message = (ready.reason or "") .. ": " .. (ready.message or "") end
return hs
```

Two more lines for Argo users. The status subresource is enabled, so status writes never bump `metadata.generation`
and Argo ignores status in diffs by default. Scans triggered manually need the annotation excluded:

```yaml
spec:
  ignoreDifferences:
  - group: db.atlasgo.io
    kind: AtlasSecurityScan
    jsonPointers: ["/metadata/annotations/db.atlasgo.io~1scan-requested-at"]
```

No sync-wave annotations are needed between a scan and the resources it watches; triggers are evaluated from revisions,
not from event order.

### kube-state-metrics

```yaml
kind: CustomResourceStateMetrics
spec:
  resources:
  - groupVersionKind: {group: db.atlasgo.io, version: v1alpha1, kind: AtlasSecurityScan}
    metricNamePrefix: atlas_securityscan
    labelsFromPath: {namespace: [metadata, namespace], name: [metadata, name]}
    metrics:
    - name: findings
      help: Non-waived findings by Security Graph level; all four levels are always present
      each: {type: Gauge, gauge: {path: [status, summary, levels], labelsFromPath: {level: [level]}, valueFrom: [count]}}
    - name: findings_total
      each: {type: Gauge, gauge: {path: [status, summary, total]}}
    - name: findings_waived
      each: {type: Gauge, gauge: {path: [status, summary, waived]}}
    - name: last_scan_trigger
      each: {type: StateSet, stateSet: {path: [status, lastScan, trigger], labelName: trigger, list: [Spec, Manual, Apply, Schedule, Policy]}}
    - name: last_success_timestamp_seconds
      each: {type: Gauge, gauge: {path: [status, lastSuccessfulTime]}}     # RFC3339 converted to epoch
    - name: next_schedule_timestamp_seconds
      each: {type: Gauge, gauge: {path: [status, nextScheduleTime]}}       # absent while suspended or unscheduled
    - name: suspended
      each: {type: Gauge, gauge: {path: [spec, suspend], nilIsZero: true}}
    - name: failed
      each: {type: Gauge, gauge: {path: [status, failed]}}
    - name: status_condition
      help: 1 for True, 0 for False and Unknown; filter by reason to tell them apart
      each: {type: Gauge, gauge: {path: [status, conditions], labelsFromPath: {type: [type], reason: [reason]}, valueFrom: [status]}}
```

The highest level is derived in PromQL from `findings{level=...} > 0`; a StateSet over `highestLevel` would produce no
series for a clean database, since the field is omitted then. kube-state-metrics maps `Unknown` to 0, the same as
`False`, so the policy alert filters by reason. When a resource stalls, `Compliant` moves to `Unknown/ReportStale` and
the policy alert resolves by design; the Stalled alert takes over.

Per-vulnerability series are possible because the report is a separate object with a top-level list, but they put CVE
ids and extension names into Prometheus, which the RBAC story keeps out of `view`. Add the second resource only where the
Prometheus audience may see them, and bind the kube-state-metrics ServiceAccount to the report viewer ClusterRole:

```yaml
  - groupVersionKind: {group: db.atlasgo.io, version: v1alpha1, kind: AtlasSecurityReport}   # opt-in
    metricNamePrefix: atlas_securityreport
    labelsFromPath: {namespace: [metadata, namespace], name: [metadata, name]}
    metrics:
    - name: vulnerability_info
      each: {type: Info, info: {path: [report, vulnerabilities], labelsFromPath: {id: [id], extension: [extension], level: [level]}}}
```

```yaml
groups:
- name: atlas-securityscan
  rules:
  - alert: AtlasDatabaseOutOfSecurityPolicy
    expr: atlas_securityscan_status_condition{type="Compliant",reason="PolicyViolated"} == 0
    labels: {severity: critical}
    annotations:
      summary: "{{ $labels.namespace }}/{{ $labels.name }} has findings at or above its failOn level"
      description: "kubectl -n {{ $labels.namespace }} get atlassecurityreport {{ $labels.name }} -o yaml"
  - alert: AtlasSecurityScanOverdue
    expr: (time() - atlas_securityscan_next_schedule_timestamp_seconds) > 3600
          and on(namespace, name) atlas_securityscan_suspended == 0
    for: 5m
    labels: {severity: warning}
    annotations:
      summary: "Scheduled scan for {{ $labels.namespace }}/{{ $labels.name }} is more than 1h overdue"
  - alert: AtlasSecurityScanStalled
    expr: atlas_securityscan_status_condition{type="Stalled"} == 1
    for: 10m
    labels: {severity: warning}
    annotations:
      summary: "{{ $labels.namespace }}/{{ $labels.name }} needs attention ({{ $labels.reason }})"
  - alert: AtlasSecurityReportStale
    expr: (time() - atlas_securityscan_last_success_timestamp_seconds) > 7 * 86400
          and on(namespace, name) atlas_securityscan_suspended == 0
    for: 1h
    labels: {severity: info}
```

### Events

Messages are fixed text or counts. The recorder aggregates repeats.

| Type | Reason | When | Message |
|---|---|---|---|
| Normal | Scanned | success | `trigger=Apply AtlasSchema/app-schema: 3 findings (1 CRITICAL, 2 HIGH) in 4 extensions; 1 waived; report atlassecurityreport/app-db` |
| Warning | PolicyViolated | success, `Compliant=False` | `3 findings at or above HIGH, highest CRITICAL` |
| Normal | MissedSchedule | catch-up covered skipped slots | `caught up 2 missed slots; latest covered 2026-09-10T01:00:00Z` |
| Warning | ScanFailed, LoginFailed, CLIError, ReadingInputs, StoringReport | attempt failed | `the database could not be scanned; attempt 3 of 20; see the operator log` |
| Warning | BackoffLimitExceeded | `failed > backoffLimit` | `backoff limit exceeded; one attempt per new slot, apply, request, spec change or waiver expiry` |
| Warning | InvalidSchedule, InvalidTimeZone, InvalidTarget | permanent validation failure | `schedule does not fire`, `unknown time zone`, `configuration yields 2 targets; one database per resource`, `security.cve.ignore is set by spec.policy and must not appear in the custom atlas.hcl` |
| Warning | TriggerNotFound | a trigger did not resolve at scan start | `AtlasSchema/app-schema not found in payments; it cannot trigger scans until it exists and is managed by this operator instance` |
| Warning | ScanWarning | the CLI reported an error but the target was scanned | `the CLI reported an error after producing the report; see the operator log` |
| Normal | Suspended, Resumed | on transition | `scanning suspended` / `scanning resumed` |

No per-finding events, no per-attempt "started" events. No CVE ids, extension names, URL-derived text or error output
in any event.

## Security and RBAC

Threat model: the Helm chart aggregates read access to the operator's kinds into the built-in `view` ClusterRole, so
assume the scan's status and Events are readable by most humans and many controllers.

- Never in scan status, report metadata, or Events: the URL in any form (even password-redacted it carries user, host,
  port, database and query), resolved `credentials` parts, CLI or driver output of any kind, CVE ids the user did not
  write themselves, extension names. Errors are classified into a closed set of reasons with fixed messages; the text
  goes to the operator log only.
- The report holds the extension inventory and the vulnerability list. It is **not** aggregated into `view` or `edit`.
  A dedicated ClusterRole `<release>-securityreport-viewer` grants `get`, `list`, `watch` on `atlassecurityreports`; it is
  aggregated into `admin` by default and bindable directly for security teams and for kube-state-metrics.
- The scan reads the Secrets it names and writes none. The controller markers request `get;list;watch` so the
  informer-backed client works when `WATCH_SECRETS=true`; the chart gates `list`/`watch` behind
  `rbac.clusterWideSecretAccess` exactly as for the other kinds. The sibling markers that request `create` on Secrets are
  not copied.
- The token and anything derived from it never appear in status, Events or logs.

Controller markers:

```go
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityreports,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas;atlasmigrations,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets;configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
```

## Chart and deployment changes

- `templates/crds/crd.yaml`: both CRDs, generated by the existing `make` target.
- `templates/manager-rbac.yaml` (hand-maintained): rules for `atlassecurityscans`, `/status`, `/finalizers` and
  `atlassecurityreports`. Secret rules stay gated by `rbac.clusterWideSecretAccess`.
- `templates/rbac.yaml` (aggregation): `atlassecurityscans` and `atlassecurityscans/status` join the `-view` and `-edit`
  roles exactly like the siblings. `atlassecurityreports` joins neither.
- New `templates/securityreport-rbac.yaml`, rendered under `rbac.create`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "atlas-operator.fullname" . }}-securityreport-viewer
  labels:
    {{- if .Values.rbac.securityReports.aggregateToAdmin }}
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
    {{- end }}
    {{- if .Values.rbac.securityReports.aggregateToEdit }}
    rbac.authorization.k8s.io/aggregate-to-edit: "true"
    {{- end }}
    {{- if .Values.rbac.securityReports.aggregateToView }}
    rbac.authorization.k8s.io/aggregate-to-view: "true"
    {{- end }}
rules:
- apiGroups: ["db.atlasgo.io"]
  resources: ["atlassecurityreports"]
  verbs: ["get", "list", "watch"]
```

```yaml
# values.yaml
rbac:
  securityReports:
    aggregateToAdmin: true
    aggregateToEdit: false
    aggregateToView: false
```

- `cmd/main.go`: register the controller; `AllowCustomConfig()` wiring as for the siblings; both kinds in
  `cacheOptions.ByObject`; `import _ "time/tzdata"`.
- `config/rbac/`: `atlassecurityscan_{editor,viewer}_role.yaml`, `atlassecurityreport_viewer_role.yaml`;
  `config/samples/db_v1alpha1_atlassecurityscan.yaml`.
- Dependencies: bump `ariga.io/atlas` to a build with `atlasexec.SecurityScan`; add `github.com/robfig/cron/v3`; add
  `SecurityScan` to the `AtlasExec` interface in `internal/controller/common.go` and to the test fake.
- README: a "Security scanning" section, a third Reason table, the `allowCustomConfig` note for `security {}` and
  `notify {}` blocks, the `labelSelector`/`watchNamespaces` note that triggers must be managed by the same instance,
  and the minimum kubectl version for `--for=jsonpath` expressions containing `=`.

## Alternatives considered

| Alternative | Why not |
|---|---|
| Full vulnerability list inline in scan status | Readable by `view`; variable size; kube-state-metrics cannot export a nested list; no tooling consumer needs it. |
| Report in a Secret | Hidden from `view`, but base64 blobs are hostile to `kubectl`, "non-credential data in Secrets" violates policy in many clusters, needs `create`/`update` on Secrets that the chart is actively shedding, and metrics cannot read it. |
| Report in a ConfigMap | `view` reads ConfigMaps; nothing gained. |
| Report history (`historyLimit`) | Trend reporting is a non-goal; one replaced-in-place report keeps garbage collection trivial. Can be added later without a breaking change. |
| Timestamp watermark (`lastApplied` newer than the scan) | Compares two clocks; stamped by no-op applies; the failure mode that motivated this design. |
| A `changeSequence` counter on AtlasSchema and AtlasMigration | Closes the drift-repair, non-linear and partial-apply gaps exactly, but couples the design to changes in two stable kinds. Listed as an open question with the existing tokens as the fallback. |
| Discovering triggers by comparing resolved URLs or Secrets | Requires reading other resources' Secrets, is heuristic (aliases, different users), is invisible in the spec, cannot be validated at admission. |
| Label selector for triggers | Reasonable future extension; explicit references index trivially and read plainly. |
| A policy kind that selects many databases | A database with no Atlas resource could not be scanned; credentials named by other teams' resources would be reused; status proportional to the selection. |
| A `security` block on AtlasSchema or AtlasMigration | A database is often managed by several resources; scan cadence, threshold and RBAC are independent of any one of them; a failed scan should not flip an apply resource's `Ready`. |
| A CronJob running the Atlas image | Delivers the schedule and the CLI's `notify` block, but not change triggering, in-cluster status, or the Secret handling the operator already has. |
| Interval schedules (`@every`, `intervalSeconds`) | Drift and cannot be planned around. |
| `startingDeadlineSeconds`, `concurrencyPolicy` | One scan per key at a time; catch-up is always exactly one scan; nothing to configure. |
| Passing `--fail-on` to the CLI | The exit code conflates "threshold reached" and "target not scanned"; per-level counts are needed anyway, so the operator grades the report. |
| Re-grading the stored report on a policy edit or waiver expiry without a scan | Would need the unfiltered report stored (no `--min-severity` to the CLI) and a second provenance for the verdict; a scan takes seconds. Accepted cost: a waiver that expires while the database is unreachable stands until a scan succeeds. |
| Waiver expiry detected by comparing the expiry with the last scan's timestamps | Not reconstructible from status after a failed attempt, since `lastScan` describes the most recent attempt; recording the set of waivers in force is a revision comparison like the rest of the design. |
| Resetting `failed` at every new schedule slot | For schedules shorter than the backoff run (about 17 minutes at the default 20 retries) `backoffLimit` would never be reached; for longer ones the resource would flap between Retrying and Stalled at every slot. One attempt per new input set while stalled keeps the alert stable and still self-heals. |
| Redacting CLI and driver errors by string replacement | Resolved IPs, driver-reformatted DSN parts and certificate names are not byte-equal to the Secret values; only a closed set of fixed messages is safe. |
| `Ready=False` on a policy violation | kstatus InProgress or Failed on a CVE a re-sync cannot fix; a separate condition keeps controller health and the verdict apart. |
| Degraded in Argo CD on a policy violation by default | Fails multi-wave syncs and stops auto-sync retries for an unrelated cause; exposed as a one-line Lua flag instead. |
| `Reconciling=True` during every scan | Flips Argo to Progressing and kstatus to InProgress on every routine scan; kstatus's Reconciling means converging on a new spec, which only `Spec`-triggered scans are. |
| A default `failOn` | A silent default would make `Compliant=False` appear after an upgrade; `Unknown/NoThreshold` is explicit. |

## Testing

- CRD schema and CEL: the rules the controller also enforces (`TZ=`, `@every`, an unparsable or never-firing schedule,
  an unknown zone) are unit-tested against `validate`; the rest are exercised against a real API server in the e2e
  script, which is the only place in this repo that runs one. `failOn` below `minSeverity` is rejected there; add
  `Local` and "neither schedule nor triggers", which nothing else backstops.
- Schedule unit tests with an injectable clock: `Next` and `latestSlotAtOrBefore` in UTC and `Europe/Berlin` across the
  spring-forward gap and the fall-back hour (two slots); `@daily`; an expression that does not fire stalls; first-run
  anchor; one due after simulated downtime of 1 and 1000 slots; the 100 000-step cap; `wake` is always positive and never
  reads `nextScheduleTime`; a scan that runs across a slot covers it, before and after the first slot is committed, and
  a schedule finer than the scan stays paced by the schedule; a waiver that lapses mid-scan requeues at once; a failed
  attempt that ran across a slot is requeued for it rather than skipping a period.
- Controller unit tests with a fake `AtlasExec` whose `SecurityScan` can return a report, fail, or mutate a trigger's
  revision while "scanning": exactly one follow-up scan then none; due-check precedence; `failed` resets only on success;
  a stalled resource makes one attempt at the next slot with conditions unchanged, and none for the same pending inputs;
  a stalled resource with a pending manual request still attempts at the next slot; suspend then resume covers a pending
  manual request and a lapsed waiver; a withdrawn request during retries restores `Ready=True`; a failed scan leaves
  `Compliant` and every watermark untouched; a report with zero or two targets; no CLI or driver text and no
  Secret-supplied URL, user, host or database anywhere in status, report or events.
- envtest: index and `EnqueueRequestsFromMapFunc` fan-out; the predicate drops a `last_applied`-only update and an
  unapplied resource, and passes the add of a recreated one, whose new uid makes it pending even at the same revision; owner
  reference garbage collection of the report; report labels copied from the scan; a pre-existing unlabelled report is
  updated, not collided with.
- Argo: `health_test.yaml` fixtures for each row of the condition table.
- Chart: `helm template` golden files for the RBAC rules under each `securityReports.aggregateTo*` combination and
  `clusterWideSecretAccess=false`.
- e2e, gated on an Atlas Pro token: a PostgreSQL target with a vulnerable extension version yields `Compliant=False`; a
  waiver flips it to True; an expired waiver flips it back; `kubectl wait` on both conditions and on
  `lastHandledScanRequest`.

## Open questions

1. Should AtlasSchema and AtlasMigration gain `status.changeSequence` (incremented only when an apply modified the
   database) so drift-repairing, non-linear and partially failed applies trigger scans? Backward-compatible, but touches
   both kinds.
2. Scan timeout: a fixed 10 minutes in-process, or `spec.timeoutSeconds`?
3. Atlas Cloud rate limits for the Security Graph: does a burst of scans across many databases need a manager-wide
   semaphore beyond `MaxConcurrentReconciles`?
4. Should `Compliant` also degrade to `Unknown/ReportStale` on age alone (for example a report older than twice the
   schedule period), or is the Prometheus alert sufficient?
5. Should a scan deleted and recreated under the same name reuse the old report object until garbage collection
   removes it, or should the report name carry the scan's uid to avoid the short dangling window?
6. The `notify {}` block in a custom `atlas.hcl` versus Kubernetes-native alerting: both work; the operator does not
   duplicate notification. Document, or discourage one?
