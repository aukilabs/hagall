package models

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSequentialIDGeneratorNew(t *testing.T) {
	t.Run("returns a new id", func(t *testing.T) {
		var idGen SequentialIDGenerator

		for i := 1; i <= 5; i++ {
			id := idGen.New()
			require.Equal(t, uint32(i), id)
		}
	})

	t.Run("returns a reusable id", func(t *testing.T) {
		var idGen SequentialIDGenerator

		for i := 1; i <= 5; i++ {
			idGen.New()
		}

		idGen.Reuse(2)
		id := idGen.New()
		require.Equal(t, uint32(2), id)
	})
}
