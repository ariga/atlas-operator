package atlas

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type (
	// Client is a client for the Atlas CLI.
	Client struct {
		path string
	}
	// ApplyParams are the parameters for the `migrate apply` command.
	ApplyParams struct {
		DirURL          string
		URL             string
		RevisionsSchema string
		BaselineVersion string
		TxMode          string
		Amount          uint64
	}
	// StatusParams are the parameters for the `migrate status` command.
	StatusParams struct {
		DirURL          string
		URL             string
		RevisionsSchema string
	}
	// LintParams are the parameters for the `migrate lint` command.
	LintParams struct {
		ConfigURL string
		DevURL    string
		DirURL    string
		Latest    uint64
		Vars      Vars
	}
	// SchemaApplyParams are the parameters for the `schema apply` command.
	SchemaApplyParams struct {
		ConfigURL string
		DevURL    string
		DryRun    bool
		Exclude   []string
		Schema    []string
		To        string
		URL       string
		Vars      Vars
	}
	// SchemaInspectParams are the parameters for the `schema inspect` command.
	SchemaInspectParams struct {
		ConfigURL string
		DevURL    string
		Exclude   []string
		Format    string
		Schema    []string
		URL       string
	}
	Vars map[string]string
)

// NewClient returns a new Atlas client.
// The client will try to find the Atlas CLI in the current directory,
// and in the PATH.
func NewClient(dir, name string) (*Client, error) {
	path, err := execPath(dir, name)
	if err != nil {
		return nil, err
	}
	return NewClientWithPath(path), nil
}

// NewClientWithPath returns a new Atlas client with the given atlas-cli path.
func NewClientWithPath(path string) *Client {
	return &Client{path: path}
}

// Apply runs the 'migrate apply' command.
func (c *Client) Apply(ctx context.Context, data *ApplyParams) (*ApplyReport, error) {
	dir, err := filepath.Abs(data.DirURL)
	if err != nil {
		return nil, err
	}
	args := []string{
		"migrate", "apply",
		"--log", "{{ json . }}",
		"--url", data.URL,
		"--dir", fmt.Sprintf("file://%s", dir),
	}
	if data.RevisionsSchema != "" {
		args = append(args, "--revisions-schema", data.RevisionsSchema)
	}
	if data.BaselineVersion != "" {
		args = append(args, "--baseline", data.BaselineVersion)
	}
	if data.TxMode != "" {
		args = append(args, "--tx-mode", data.TxMode)
	}
	if data.Amount > 0 {
		args = append(args, strconv.FormatUint(data.Amount, 10))
	}
	var report ApplyReport
	if _, err := c.runCommand(ctx, args, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// SchemaApply runs the 'schema apply' command.
func (c *Client) SchemaApply(ctx context.Context, data *SchemaApplyParams) (*SchemaApply, error) {
	args := []string{
		"schema", "apply",
		"--format", "{{ json . }}",
		"--url", data.URL,
		"--to", data.To,
	}
	if data.ConfigURL != "" {
		args = append(args, "-c", data.ConfigURL, "--env", "k8s")
	}
	if data.DryRun {
		args = append(args, "--dry-run")
	} else {
		args = append(args, "--auto-approve")
	}
	if data.DevURL != "" {
		args = append(args, "--dev-url", data.DevURL)
	}
	if len(data.Schema) > 0 {
		args = append(args, "--schema", strings.Join(data.Schema, ","))
	}
	if len(data.Exclude) > 0 {
		args = append(args, "--exclude", strings.Join(data.Exclude, ","))
	}
	args = append(args, data.Vars.AsArgs()...)
	var result SchemaApply
	if _, err := c.runCommand(ctx, args, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SchemaInspect runs the 'schema inspect' command.
func (c *Client) SchemaInspect(ctx context.Context, data *SchemaInspectParams) (string, error) {
	args := []string{
		"schema", "inspect",
		"--url", data.URL,
	}
	if data.DevURL != "" {
		args = append(args, "--dev-url", data.DevURL)
	}
	if data.Format == "sql" {
		args = append(args, "--format", "{{ sql . }}")
	}
	if len(data.Schema) > 0 {
		args = append(args, "--schema", strings.Join(data.Schema, ","))
	}
	if len(data.Exclude) > 0 {
		args = append(args, "--exclude", strings.Join(data.Exclude, ","))
	}
	return c.runCommand(ctx, args, nil)
}

// Lint runs the 'migrate lint' command.
func (c *Client) Lint(ctx context.Context, data *LintParams) (*SummaryReport, error) {
	args := []string{
		"migrate", "lint", "--log", "{{ json . }}",
		"--dev-url", data.DevURL,
		"--dir", data.DirURL,
	}
	if data.Latest > 0 {
		args = append(args, "--latest", strconv.FormatUint(data.Latest, 10))
	}
	if data.ConfigURL != "" {
		args = append(args, "-c", data.ConfigURL, "--env", "k8s")
	}
	args = append(args, data.Vars.AsArgs()...)
	var report SummaryReport
	if _, err := c.runCommand(ctx, args, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// Status runs the 'migrate status' command.
func (c *Client) Status(ctx context.Context, data *StatusParams) (*StatusReport, error) {
	dir, err := filepath.Abs(data.DirURL)
	if err != nil {
		return nil, err
	}
	args := []string{
		"migrate", "status", "--log", "{{ json . }}",
		"--url", data.URL,
		"--dir", fmt.Sprintf("file://%s", dir),
	}
	if data.RevisionsSchema != "" {
		args = append(args, "--revisions-schema", data.RevisionsSchema)
	}
	var report StatusReport
	if _, err := c.runCommand(ctx, args, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// runCommand runs the given command and unmarshals the output into the given
// interface.
func (c *Client) runCommand(ctx context.Context, args []string, report interface{}) (string, error) {
	cmd := exec.CommandContext(ctx, c.path, args...)
	cmd.Env = append(cmd.Env, "ATLAS_NO_UPDATE_NOTIFIER=1")
	output, err := cmd.Output()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return string(output), err
		}
		if exitErr.Stderr != nil && len(exitErr.Stderr) > 0 {
			return string(output), &cliError{
				summary: string(exitErr.Stderr),
				detail:  string(output),
			}
		}
		if exitErr.ExitCode() != 1 || !json.Valid(output) {
			return string(output), &cliError{
				summary: "Atlas CLI",
				detail:  string(output),
			}
		}
		// When the exit code is 1, it means that the command
		// was executed successfully, and the output is a JSON
	}
	if report != nil {
		if err := json.Unmarshal(output, report); err != nil {
			return string(output), fmt.Errorf("atlas: unable to decode the report %w", err)
		}
	}
	return string(output), nil
}

// LatestVersion returns the latest version of the migrations directory.
func (r StatusReport) LatestVersion() string {
	if l := len(r.Available); l > 0 {
		return r.Available[l-1].Version
	}
	return ""
}

// Amount returns the number of migrations need to apply
// for the given version.
//
// The second return value is true if the version is found
// and the database is up-to-date.
//
// If the version is not found, it returns 0 and the second
// return value is false.
func (r StatusReport) Amount(version string) (amount uint64, ok bool) {
	if version == "" {
		amount := uint64(len(r.Pending))
		return amount, amount == 0
	}
	if r.Current == version {
		return amount, true
	}
	for idx, v := range r.Pending {
		if v.Version == version {
			amount = uint64(idx + 1)
			break
		}
	}
	return amount, false
}

func execPath(dir, name string) (file string, err error) {
	file = filepath.Join(dir, name)
	if _, err = os.Stat(file); err == nil {
		return file, nil
	}
	// If the binary is not in the current directory,
	// try to find it in the PATH.
	return exec.LookPath(name)
}

type cliError struct {
	summary string
	detail  string
}

// Error implements the error interface.
func (e cliError) Error() string {
	return e.summary
}

// Summary implements the diag.Diagnostic interface.
func (e cliError) Summary() string {
	if strings.HasPrefix(e.summary, "Error: ") {
		return e.summary[7:]
	}
	return e.summary
}

// Detail implements the diag.Diagnostic interface.
func (e cliError) Detail() string {
	return strings.TrimSpace(e.detail)
}

func TempFile(content, ext string) (string, func() error, error) {
	f, err := os.CreateTemp("", "atlas-k8s-*."+ext)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("file://%s", f.Name()), func() error {
		return os.Remove(f.Name())
	}, nil
}

func (v Vars) AsArgs() []string {
	var args []string
	for k, v := range v {
		args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
	}
	return args
}
