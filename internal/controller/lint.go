// Copyright 2023 The Atlas Operator Authors.
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
	"context"
	"fmt"
	"os"
	"strings"

	"ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/sqlcheck"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const lintDirName = "lint-migrations"

// lint run `atlas migrate lint` to check for destructive changes.
// It returns a destructiveErr if destructive changes are detected.
//
// It works by creating two versions of migration:
// - 1.sql: the current schema.
// - 2.sql: the pending changes.
// Then it runs `atlas migrate lint` in the temporary directory.
func (r *AtlasSchemaReconciler) lint(ctx context.Context, wd *atlasexec.WorkingDir, data *managedData, vars atlasexec.VarArgs) error {
	cli, err := r.atlasClient(wd.Path(), data.Cloud)
	if err != nil {
		return err
	}
	current, err := cli.SchemaInspect(ctx, &atlasexec.SchemaInspectParams{
		Env:    data.EnvName,
		Format: "{{ sql . }}",
	})
	if err != nil {
		return err
	}
	plan, err := cli.SchemaApply(ctx, &atlasexec.SchemaApplyParams{
		Env:    data.EnvName,
		To:     data.Desired.String(),
		DryRun: true, // Dry run to get pending changes.
	})
	if err != nil {
		return err
	}
	defer func() {
		dir := wd.Path(lintDirName)
		if err := os.RemoveAll(dir); err != nil {
			log.FromContext(ctx).Error(err,
				"unable to remove temporary directory", "dir", dir)
		}
	}()
	dir, err := memDir(map[string]string{
		"1.sql": current,
		"2.sql": strings.Join(plan.Changes.Pending, ";\n"),
	})
	if err != nil {
		return err
	}
	err = wd.CopyFS(lintDirName, dir)
	if err != nil {
		return err
	}
	lint, err := cli.MigrateLint(ctx, &atlasexec.MigrateLintParams{
		Env:    data.EnvName,
		DirURL: fmt.Sprintf("file://./%s", lintDirName),
		Latest: 1, // Only lint 2.sql, pending changes.
		Vars:   vars,
	})
	if err != nil {
		return err
	}
	if diags := destructive(lint); len(diags) > 0 {
		return &destructiveErr{diags: diags}
	}
	return nil
}

func destructive(rep *atlasexec.SummaryReport) (checks []sqlcheck.Diagnostic) {
	for _, f := range rep.Files {
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

type destructiveErr struct {
	diags []sqlcheck.Diagnostic
}

func (d *destructiveErr) Error() string {
	var buf strings.Builder
	buf.WriteString("destructive changes detected:\n")
	for _, diag := range d.diags {
		buf.WriteString("- " + diag.Text + "\n")
	}
	return buf.String()
}

func (d *destructiveErr) FirstRun() (string, string) {
	return "FirstRunDestructive", d.Error() + "\n" +
		"To prevent accidental drop of resources, first run of a schema must not contain destructive changes.\n" +
		"Read more: https://atlasgo.io/integrations/kubernetes/#destructive-changes"
}
