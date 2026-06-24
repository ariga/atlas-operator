package controller

import (
	"testing"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"

	"github.com/stretchr/testify/require"
)

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
