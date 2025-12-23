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

	"ariga.io/atlas/atlasexec"
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
	plans, err := cli.SchemaApplySlice(ctx, &atlasexec.SchemaApplyParams{
		Env:    data.EnvName,
		Vars:   vars,
		To:     data.targetURL(),
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
	if len(plans) != 1 {
		return fmt.Errorf("unexpected number of schema plans: %d", len(plans))
	}
	var causes []string
	for _, c := range plans[0].Changes.Pending {
		if strings.Contains(c, "DROP ") {
			causes = append(causes, c)
		}
	}
	if len(causes) > 0 {
		return &destructiveErr{clauses: causes}
	}
	return nil
}

type destructiveErr struct {
	clauses []string
}

func (d *destructiveErr) Error() string {
	var buf strings.Builder
	buf.WriteString("destructive changes detected:\n")
	for _, c := range d.clauses {
		buf.WriteString("- " + c + "\n")
	}
	return buf.String()
}

func (d *destructiveErr) FirstRun() (string, string) {
	return "FirstRunDestructive", d.Error() + "\n" +
		"To prevent accidental drop of resources, first run of a schema must not contain destructive changes.\n" +
		"Read more: https://atlasgo.io/integrations/kubernetes/#destructive-changes"
}
