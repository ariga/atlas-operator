package atlas

import (
	"time"

	"ariga.io/atlas/sql/sqlcheck"
	"ariga.io/atlas/sql/sqlclient"
)

type (
	// File wraps migrate.File to implement json.Marshaler.
	File struct {
		Name        string `json:"Name,omitempty"`
		Version     string `json:"Version,omitempty"`
		Description string `json:"Description,omitempty"`
	}
	// AppliedFile is part of an ApplyReport containing information about an applied file in a migration attempt.
	AppliedFile struct {
		File
		Start   time.Time
		End     time.Time
		Skipped int      // Amount of skipped SQL statements in a partially applied file.
		Applied []string // SQL statements applied with success
		Error   *struct {
			SQL   string // SQL statement that failed.
			Error string // Error returned by the database.
		}
	}
	// ApplyReport contains a summary of a migration applying attempt on a database.
	ApplyReport struct {
		Pending []File         `json:"Pending,omitempty"` // Pending migration files
		Applied []*AppliedFile `json:"Applied,omitempty"` // Applied files
		Current string         `json:"Current,omitempty"` // Current migration version
		Target  string         `json:"Target,omitempty"`  // Target migration version
		Start   time.Time
		End     time.Time
		// Error is set even then, if it was not caused by a statement in a migration file,
		// but by Atlas, e.g. when committing or rolling back a transaction.
		Error string `json:"Error,omitempty"`
	}
	// A Revision denotes an applied migration in a deployment. Used to track migration executions state of a database.
	Revision struct {
		Version         string        `json:"Version"`             // Version of the migration.
		Description     string        `json:"Description"`         // Description of this migration.
		Type            string        `json:"Type"`                // Type of the migration.
		Applied         int           `json:"Applied"`             // Applied amount of statements in the migration.
		Total           int           `json:"Total"`               // Total amount of statements in the migration.
		ExecutedAt      time.Time     `json:"ExecutedAt"`          // ExecutedAt is the starting point of execution.
		ExecutionTime   time.Duration `json:"ExecutionTime"`       // ExecutionTime of the migration.
		Error           string        `json:"Error,omitempty"`     // Error of the migration, if any occurred.
		ErrorStmt       string        `json:"ErrorStmt,omitempty"` // ErrorStmt is the statement that raised Error.
		OperatorVersion string        `json:"OperatorVersion"`     // OperatorVersion that executed this migration.
	}
	// StatusReport contains a summary of the migration status of a database.
	StatusReport struct {
		Available []File      `json:"Available,omitempty"` // Available migration files
		Pending   []File      `json:"Pending,omitempty"`   // Pending migration files
		Applied   []*Revision `json:"Applied,omitempty"`   // Applied migration files
		Current   string      `json:"Current,omitempty"`   // Current migration version
		Next      string      `json:"Next,omitempty"`      // Next migration version
		Count     int         `json:"Count,omitempty"`     // Count of applied statements of the last revision
		Total     int         `json:"Total,omitempty"`     // Total statements of the last migration
		Status    string      `json:"Status,omitempty"`    // Status of migration (OK, PENDING)
		Error     string      `json:"Error,omitempty"`     // Last Error that occurred
		SQL       string      `json:"SQL,omitempty"`       // SQL that caused the last Error
	}
	// FileReport contains a summary of the analysis of a single file.
	FileReport struct {
		Name    string            `json:"Name,omitempty"`    // Name of the file.
		Text    string            `json:"Text,omitempty"`    // Contents of the file.
		Reports []sqlcheck.Report `json:"Reports,omitempty"` // List of reports.
		Error   string            `json:"Error,omitempty"`   // File specific error.
	}
	// A SummaryReport contains a summary of the analysis of all files.
	// It is used as an input to templates to report the CI results.
	SummaryReport struct {
		// Steps of the analysis. Added in verbose mode.
		Steps []struct {
			Name   string `json:"Name,omitempty"`   // Step name.
			Text   string `json:"Text,omitempty"`   // Step description.
			Error  string `json:"Error,omitempty"`  // Error that cause the execution to halt.
			Result any    `json:"Result,omitempty"` // Result of the step. For example, a diagnostic.
		}

		// Schema versions found by the runner.
		Schema struct {
			Current string `json:"Current,omitempty"` // Current schema.
			Desired string `json:"Desired,omitempty"` // Desired schema.
		}

		// Files reports. Non-empty in case there are findings.
		Files []*FileReport `json:"Files,omitempty"`
	}
	// StmtError groups a statement with its execution error.
	StmtError struct {
		Stmt string `json:"Stmt,omitempty"` // SQL statement that failed.
		Text string `json:"Text,omitempty"` // Error message as returned by the database.
	}
	// Env holds the environment information.
	Env struct {
		Driver string         `json:"Driver,omitempty"` // Driver name.
		URL    *sqlclient.URL `json:"URL,omitempty"`    // URL to dev database.
		Dir    string         `json:"Dir,omitempty"`    // Path to migration directory.
	}
	// Changes represents a list of changes that are pending or applied.
	Changes struct {
		Applied []string   `json:"Applied,omitempty"` // SQL changes applied with success
		Pending []string   `json:"Pending,omitempty"` // SQL changes that were not applied
		Error   *StmtError `json:"Error,omitempty"`   // Error that occurred during applying
	}
	// SchemaApply contains a summary of a 'schema apply' execution on a database.
	SchemaApply struct {
		Env
		Changes Changes `json:"Changes,omitempty"`
		// General error that occurred during execution.
		// e.g., when committing or rolling back a transaction.
		Error string `json:"Error,omitempty"`
	}
)
