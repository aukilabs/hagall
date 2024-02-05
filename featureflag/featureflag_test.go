package featureflag

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFeatureFlag(t *testing.T) {
	f := New([]string{"feature1"})

	t.Run("run if enabled", func(t *testing.T) {
		var runFeature1 bool
		f.IfSet("feature1", func() {
			runFeature1 = true
		})
		require.True(t, runFeature1)

		var runFeature2 bool
		f.IfSet("feature2", func() {
			runFeature2 = true
		})
		require.False(t, runFeature2)
	})

	t.Run("run if disabled", func(t *testing.T) {
		var runFeature1 bool
		f.IfNotSet("feature1", func() {
			runFeature1 = true
		})
		require.False(t, runFeature1)

		var runFeature2 bool
		f.IfNotSet("feature2", func() {
			runFeature2 = true
		})
		require.True(t, runFeature2)
	})
}
