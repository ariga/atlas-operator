package controllers

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"

	atlas "ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/sqlcheck"
)

func (r *AtlasSchemaReconciler) lint(ctx context.Context, des *managedData, devURL string, vars ...atlas.Vars) error {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "conf.tmpl", des.policy); err != nil {
		return err
	}
	lintcfg, cleancfg, err := atlas.TempFile(buf.String(), "hcl")
	if err != nil {
		return err
	}
	defer cleancfg()
	tmpdir, err := os.MkdirTemp("", "run-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	ins, err := r.cli.SchemaInspect(ctx, &atlas.SchemaInspectParams{
		DevURL: devURL,
		URL:    des.url.String(),
		Format: "sql",
		Schema: des.schemas,
	})
	if err != nil {
		return transient(err)
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "1.sql"), []byte(ins), 0644); err != nil {
		return err
	}
	desired, clean, err := atlas.TempFile(des.desired, des.ext)
	if err != nil {
		return err
	}
	defer clean()
	var vv atlas.Vars
	if len(vars) > 0 {
		vv = vars[0]
	}
	dry, err := r.cli.SchemaApply(ctx, &atlas.SchemaApplyParams{
		DryRun:  true,
		URL:     des.url.String(),
		To:      desired,
		DevURL:  devURL,
		Exclude: des.exclude,
		Schema:  des.schemas,
	})
	if isSQLErr(err) {
		return err
	}
	if err != nil {
		return transient(err)
	}
	plan := strings.Join(dry.Changes.Pending, ";\n")
	if err := os.WriteFile(filepath.Join(tmpdir, "2.sql"), []byte(plan), 0644); err != nil {
		return err
	}
	lint, err := r.cli.Lint(ctx, &atlas.LintParams{
		DevURL:    devURL,
		DirURL:    "file://" + tmpdir,
		Latest:    1,
		ConfigURL: lintcfg,
		Vars:      vv,
	})
	if isSQLErr(err) {
		return err
	}
	if err != nil {
		return transient(err)
	}
	if diags := destructive(lint.Files); len(diags) > 0 {
		return destructiveErr{diags: diags}
	}
	return nil
}

func (r *AtlasSchemaReconciler) verifyFirstRun(ctx context.Context, des *managedData, devURL string) error {
	return r.lint(ctx, des, devURL, atlas.Vars{
		"lint_destructive": "true",
	})
}

func destructive(files []*atlas.FileReport) (checks []sqlcheck.Diagnostic) {
	for _, f := range files {
		for _, r := range f.Reports {
			if f.Error == "" {
				continue
			}
			for _, diag := range r.Diagnostics {
				if strings.HasPrefix(diag.Code, "DS") {
					checks = append(checks, diag)
				}
			}
		}
	}
	return
}
