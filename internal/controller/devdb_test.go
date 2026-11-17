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
	"errors"
	"fmt"
	"maps"
	"testing"

	"ariga.io/atlas/atlasexec"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cleanExec answers `schema clean` calls with fn, and records their params.
type cleanExec struct {
	mockAtlasExec
	fn func(*atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error)
}

// SchemaClean implements AtlasExec.
func (c *cleanExec) SchemaClean(_ context.Context, p *atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
	c.schemaCleans = append(c.schemaCleans, p)
	if c.fn == nil {
		return &atlasexec.SchemaClean{}, nil
	}
	return c.fn(p)
}

// dropped returns a `schema clean` result that executed the given statements.
func dropped(stmts ...string) *atlasexec.SchemaClean {
	return &atlasexec.SchemaClean{Applied: &atlasexec.AppliedFile{Applied: stmts}}
}

func TestCleanDevDB(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		// Every call gets the URL only: no project config or env, as `schema clean`
		// falls back to the env url, the target database, when --url is not set.
		want []*atlasexec.SchemaCleanParams
	}{
		{
			name: "postgres database keeps the public schema",
			url:  "postgres://root:pass@devdb:5432/dev?sslmode=disable",
			want: []*atlasexec.SchemaCleanParams{
				{URL: "postgres://root:pass@devdb:5432/dev?sslmode=disable", Exclude: []string{"public"}, AutoApprove: true},
				{URL: "postgres://root:pass@devdb:5432/dev?search_path=public&sslmode=disable", AutoApprove: true},
			},
		},
		{
			name: "postgres schema",
			url:  "postgres://root:pass@devdb:5432/dev?search_path=app&sslmode=disable",
			want: []*atlasexec.SchemaCleanParams{
				{URL: "postgres://root:pass@devdb:5432/dev?search_path=app&sslmode=disable", AutoApprove: true},
			},
		},
		{
			name: "postgresql scheme alias",
			url:  "postgresql://devdb/dev",
			want: []*atlasexec.SchemaCleanParams{
				{URL: "postgresql://devdb/dev", Exclude: []string{"public"}, AutoApprove: true},
				{URL: "postgresql://devdb/dev?search_path=public", AutoApprove: true},
			},
		},
		{
			name: "cockroachdb database",
			url:  "crdb://root@devdb:26257/defaultdb?sslmode=disable",
			want: []*atlasexec.SchemaCleanParams{
				{URL: "crdb://root@devdb:26257/defaultdb?sslmode=disable", Exclude: []string{"public"}, AutoApprove: true},
				{URL: "crdb://root@devdb:26257/defaultdb?search_path=public&sslmode=disable", AutoApprove: true},
			},
		},
		{
			name: "horizondb database",
			url:  "horizondb://u:p@h.postgres.database.azure.com:5432/db?sslmode=require",
			want: []*atlasexec.SchemaCleanParams{
				{URL: "horizondb://u:p@h.postgres.database.azure.com:5432/db?sslmode=require", Exclude: []string{"public"}, AutoApprove: true},
				{URL: "horizondb://u:p@h.postgres.database.azure.com:5432/db?search_path=public&sslmode=require", AutoApprove: true},
			},
		},
		{
			name: "mysql",
			url:  "mysql://root:pass@devdb:3306/dev",
			want: []*atlasexec.SchemaCleanParams{{URL: "mysql://root:pass@devdb:3306/dev", AutoApprove: true}},
		},
		{
			name: "sqlite",
			url:  "sqlite://db?mode=memory",
			want: []*atlasexec.SchemaCleanParams{{URL: "sqlite://db?mode=memory", AutoApprove: true}},
		},
		{
			name: "unknown driver",
			url:  "docker://postgres/16/dev",
			want: []*atlasexec.SchemaCleanParams{{URL: "docker://postgres/16/dev", AutoApprove: true}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cli := &cleanExec{}
			r := &devDBReconciler{recorder: record.NewFakeRecorder(10)}
			r.clean(t.Context(), &dbv1alpha1.AtlasSchema{}, cli, tt.url)
			require.Equal(t, tt.want, cli.schemaCleans)
		})
	}
}

func TestDevDBReconciler_clean(t *testing.T) {
	const (
		devURL = "mysql://root:pass@devdb:3306/dev"
		realm  = "postgres://root:pass@devdb:5432/dev?sslmode=disable"
		public = "postgres://root:pass@devdb:5432/dev?search_path=public&sslmode=disable"
	)
	rec := record.NewFakeRecorder(10)
	r := &devDBReconciler{recorder: rec}
	sc := &dbv1alpha1.AtlasSchema{}
	// A clean database is not reported.
	r.clean(t.Context(), sc, &cleanExec{}, devURL)
	require.Empty(t, events(rec))
	// Dropped leftovers are reported.
	r.clean(t.Context(), sc, &cleanExec{fn: func(*atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		return dropped("DROP TABLE `t1`;", "DROP TABLE `t2`;"), nil
	}}, devURL)
	require.Equal(t, []string{"Normal CleanedDevDB Dropped leftovers from the dev database: DROP TABLE `t1`; DROP TABLE `t2`;"}, events(rec))
	// Failures are reported as warnings.
	r.clean(t.Context(), sc, &cleanExec{fn: func(*atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		return nil, errors.New("Error: permission denied\n")
	}}, devURL)
	require.Equal(t, []string{"Warning CleaningDevDB Error cleaning the dev database: Error: permission denied"}, events(rec))

	// The statements of both passes are reported together.
	r.clean(t.Context(), sc, &cleanExec{fn: func(p *atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		if p.URL == realm {
			return dropped(`DROP SCHEMA "leftover" CASCADE;`), nil
		}
		return dropped(`DROP TABLE "t1";`), nil
	}}, realm)
	require.Equal(t, []string{`Normal CleanedDevDB Dropped leftovers from the dev database: DROP SCHEMA "leftover" CASCADE; DROP TABLE "t1";`}, events(rec))
	// A database without the public schema has nothing to empty there.
	noPublic := errors.New(`Error: postgres: schema "public" was not found`)
	r.clean(t.Context(), sc, &cleanExec{fn: func(p *atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		if p.URL == realm {
			return dropped(`DROP SCHEMA "leftover" CASCADE;`), nil
		}
		return nil, noPublic
	}}, realm)
	require.Equal(t, []string{`Normal CleanedDevDB Dropped leftovers from the dev database: DROP SCHEMA "leftover" CASCADE;`}, events(rec))
	// But a missing schema set by the user is an error.
	r.clean(t.Context(), sc, &cleanExec{fn: func(*atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		return nil, noPublic
	}}, public)
	require.Equal(t, []string{`Warning CleaningDevDB Error cleaning the dev database: Error: postgres: schema "public" was not found`}, events(rec))
	// A failed pass stops the cleaning.
	cli := &cleanExec{fn: func(*atlasexec.SchemaCleanParams) (*atlasexec.SchemaClean, error) {
		return nil, errors.New("permission denied")
	}}
	r.clean(t.Context(), sc, cli, realm)
	require.Len(t, cli.schemaCleans, 1)
	require.Equal(t, []string{"Warning CleaningDevDB Error cleaning the dev database: permission denied"}, events(rec))
	// An invalid URL is not passed to Atlas, and is not reported with its credentials.
	cli = &cleanExec{}
	r.clean(t.Context(), sc, cli, "postgres://root:s3cret@devdb:port/dev")
	require.Empty(t, cli.schemaCleans)
	require.Equal(t, []string{"Warning CleaningDevDB Error cleaning the dev database: invalid URL"}, events(rec))
}

func TestShouldCleanDevDB(t *testing.T) {
	const devURL = "postgres://root:pass@devdb:5432/dev"
	optIn := &dbv1alpha1.DevDB{Clean: true}
	// Retries of a resource that was updated since its creation.
	const gen, failed = 2, 1
	require.True(t, shouldCleanDevDB(devURL, true, nil, false, gen, failed), "the dev database of the operator is cleaned on retries")
	require.False(t, shouldCleanDevDB(devURL, false, nil, false, gen, failed), "a user dev database is not cleaned by default")
	require.False(t, shouldCleanDevDB(devURL, false, &dbv1alpha1.DevDB{}, false, gen, failed))
	require.True(t, shouldCleanDevDB(devURL, false, optIn, false, gen, failed), "a user dev database is cleaned on opt-in")
	require.False(t, shouldCleanDevDB(devURL, false, optIn, true, gen, failed), "a dev URL set in the project config is never cleaned")
	require.False(t, shouldCleanDevDB(devURL, true, nil, true, gen, failed), "the project config overrides the dev database of the operator")
	require.False(t, shouldCleanDevDB("", true, optIn, false, gen, failed))
	// Only retries clean, and never in the first generation of a resource.
	require.False(t, shouldCleanDevDB(devURL, true, optIn, false, gen, 0), "the first attempt does not clean")
	require.False(t, shouldCleanDevDB(devURL, true, optIn, false, 1, failed), "retries in the first generation do not clean")
	require.True(t, shouldCleanDevDB(devURL, true, optIn, false, 3, 5))
}

func TestConfigHasDev(t *testing.T) {
	parse := func(s string) *hclwrite.File {
		f, diags := hclwrite.ParseConfig([]byte(s), "", hcl.InitialPos)
		require.False(t, diags.HasErrors(), diags.Error())
		return f
	}
	require.False(t, configHasDev(nil, defaultEnvName))
	require.True(t, configHasDev(parse(`env "kubernetes" { dev = "postgres://devdb" }`), defaultEnvName))
	require.True(t, configHasDev(parse(`env { dev = getenv("DEV_URL") }`), defaultEnvName), "unnamed env blocks apply to any env")
	require.False(t, configHasDev(parse(`env "kubernetes" { url = "postgres://db" }`), defaultEnvName))
	require.False(t, configHasDev(parse(`env "other" { dev = "postgres://devdb" }`), defaultEnvName))
}

func TestCleanConfigDev(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	config := dbv1alpha1.ProjectConfigSpec{Config: fmt.Sprintf("env %q {\n  dev = \"postgres://devdb/dev\"\n}\n", defaultEnvName)}
	// AtlasMigration.
	mg := &dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec:        dbv1alpha1.TargetSpec{URL: "postgres://db/app"},
			EnvName:           defaultEnvName,
			ProjectConfigSpec: config,
			DevDB:             &dbv1alpha1.DevDB{Clean: true},
			Dir:               dbv1alpha1.Dir{Local: map[string]string{"1.sql": "CREATE TABLE t1 (id int);"}},
		},
	}
	_, err := extractMigrationData(t.Context(), c, mg, true)
	require.ErrorIs(t, err, errCleanConfigDev)
	mg.Spec.DevDB.Clean = false
	_, err = extractMigrationData(t.Context(), c, mg, true)
	require.NoError(t, err)
	// AtlasSchema.
	r := NewAtlasSchemaReconciler(&mockManager{client: c, recorder: record.NewFakeRecorder(1), scheme: scheme}, false)
	r.AllowCustomConfig()
	config.EnvName = defaultEnvName
	sc := &dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
		Spec: dbv1alpha1.AtlasSchemaSpec{
			TargetSpec:        dbv1alpha1.TargetSpec{URL: "postgres://db/app"},
			ProjectConfigSpec: config,
			DevDB:             &dbv1alpha1.DevDB{Clean: true},
			Schema:            dbv1alpha1.Schema{SQL: "CREATE TABLE t1 (id int);"},
		},
	}
	_, err = r.extractData(t.Context(), sc)
	require.ErrorIs(t, err, errCleanConfigDev)
	sc.Spec.DevDB.Clean = false
	_, err = r.extractData(t.Context(), sc)
	require.NoError(t, err)
}

func TestDeploymentDevDB_Metadata(t *testing.T) {
	metadata := &dbv1alpha1.DevDBMetadata{
		Labels:      map[string]string{"team": "platform", labelEngine: "override", labelInstance: "override", "app.kubernetes.io/name": "override", "app.kubernetes.io/part-of": "override", "app.kubernetes.io/created-by": "override"},
		Annotations: map[string]string{"example.com/monitor": "enabled", annoConnTmpl: "override"},
	}
	labels, annotations := maps.Clone(metadata.Labels), maps.Clone(metadata.Annotations)
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "custom", Image: "postgres:18"}}}
	deploy := deploymentDevDB(types.NamespacedName{Name: "example-atlas-dev-db", Namespace: "default"}, dbv1alpha1.DriverPostgres, spec, "postgres://localhost/dev", metadata)
	require.Equal(t, map[string]string{
		labelEngine: "postgres", labelInstance: "example-atlas-dev-db",
		"app.kubernetes.io/name": "atlas-dev-db", "app.kubernetes.io/part-of": "atlas-operator", "app.kubernetes.io/created-by": "controller-manager",
	}, deploy.Spec.Selector.MatchLabels)
	expected := maps.Clone(deploy.Spec.Selector.MatchLabels)
	expected["team"] = "platform"
	require.Equal(t, expected, deploy.Spec.Template.Labels)
	require.Equal(t, map[string]string{"example.com/monitor": "enabled", annoConnTmpl: "postgres://localhost/dev"}, deploy.Spec.Template.Annotations)
	require.Equal(t, spec, deploy.Spec.Template.Spec)
	require.Equal(t, labels, metadata.Labels)
	require.Equal(t, annotations, metadata.Annotations)
}

func TestDevDBPrewarmEnabledFallsBackToGlobalDefault(t *testing.T) {
	r := &devDBReconciler{prewarm: true}
	sc := &dbv1alpha1.AtlasSchema{}

	enabled := r.prewarmEnabled(sc)
	require.True(t, enabled)
}

func TestDevDBPrewarmEnabledUsesResourceOverride(t *testing.T) {
	r := &devDBReconciler{prewarm: true}
	override := false
	sc := &dbv1alpha1.AtlasSchema{
		Spec: dbv1alpha1.AtlasSchemaSpec{
			PrewarmDevDB: &override,
		},
	}

	enabled := r.prewarmEnabled(sc)
	require.False(t, enabled)
}

func TestDevDBPrewarmEnabledUsesMigrationOverride(t *testing.T) {
	r := &devDBReconciler{prewarm: false}
	override := true
	mg := &dbv1alpha1.AtlasMigration{
		Spec: dbv1alpha1.AtlasMigrationSpec{
			PrewarmDevDB: &override,
		},
	}

	enabled := r.prewarmEnabled(mg)
	require.True(t, enabled)
}
