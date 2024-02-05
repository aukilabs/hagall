package dagaz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGridCreation(t *testing.T) {
	emptyGrid := NewRegularGrid(0, 0, 0)
	require.True(t, emptyGrid.Resolution == 1)
	require.True(t, emptyGrid.PlaneCount == 0)
	require.True(t, emptyGrid.MergeCount == 0)
	require.True(t, emptyGrid.Min.Equal(Vector3f{0, 0, 0}))
	require.True(t, emptyGrid.Max.Equal(Vector3f{1, 0, 1}))
	require.True(t, len(emptyGrid.Grid) == 1)
	require.True(t, len(emptyGrid.Grid[0]) == 1)
}

func TestGridQuadInsertion(t *testing.T) {
	grid := NewRegularGrid(0, 0, 1)

	quad := Quad{
		Center:  Vector3f{0, 0, 0},
		Extents: Vector3f{1, 0, 1},
		Normal:  Vector3f{0, 1, 0},
	}

	grid.InsertQuad(quad)
	require.True(t, grid.PlaneCount == 1)
	require.True(t, grid.MergeCount == 0)
	require.True(t, grid.Min.Equal(Vector3f{-1, 0, -1}))
	require.True(t, grid.Max.Equal(Vector3f{2, 0, 2}))
	require.True(t, len(grid.Grid) == 3)
	require.True(t, len(grid.Grid[0]) == 3)

	// verify insertion of same quad is merged:
	grid.InsertQuad(quad)
	require.True(t, grid.PlaneCount == 1)
	require.True(t, grid.MergeCount == 1)
	require.True(t, grid.Min.Equal(Vector3f{-1, 0, -1}))
	require.True(t, grid.Max.Equal(Vector3f{2, 0, 2}))
	require.True(t, len(grid.Grid) == 3)
	require.True(t, len(grid.Grid[0]) == 3)

	// verify insertion of distinct quad:
	newQuad := Quad{
		Center:  Vector3f{2, 0, 0},
		Extents: Vector3f{0.1, 0, 0.1},
		Normal:  Vector3f{0, 1, 0},
	}
	grid.InsertQuad(newQuad)
	require.True(t, grid.PlaneCount == 2)
	require.True(t, grid.MergeCount == 1)
	require.True(t, grid.Min.Equal(Vector3f{-1, 0, -1}))
	require.True(t, grid.Max.Equal(Vector3f{3, 0, 2}))
	require.True(t, len(grid.Grid) == 3)
	require.True(t, len(grid.Grid[0]) == 4)
}

func TestGridQuadIntersection(t *testing.T) {
	quad := Quad{
		Center:  Vector3f{0, 0, 0},
		Extents: Vector3f{1, 0, 1},
		Normal:  Vector3f{0, 1, 0},
	}

	grid := NewRegularGrid(1, 1, 1)
	grid.InsertQuad(quad)

	t.Run("QuadIntersection: Hit", func(t *testing.T) {
		ray := Ray{
			From: Vector3f{0, 1, 0},
			To:   Vector3f{0, -1, 0},
		}

		q, hit := grid.IntersectQuad(ray)

		require.True(t, quad == *q)
		require.True(t, 0.5 == hit)
	})

	t.Run("QuadIntersection: No Hit", func(t *testing.T) {
		ray := Ray{
			From: Vector3f{10, 1, 0},
			To:   Vector3f{0, -1, 0},
		}

		q, hit := grid.IntersectQuad(ray)

		require.True(t, nil == q)
		require.True(t, -1 == hit)
	})
}

func TestGridGetRegion(t *testing.T) {
	grid := NewRegularGrid(1, 1, 1)

	t.Run("GetRegion: Get the entire session", func(t *testing.T) {
		quad := Quad{
			Center:  Vector3f{-2, 0, -2},
			Extents: Vector3f{1, 0, 1},
			Normal:  Vector3f{0, 1, 0},
		}
		grid.InsertQuad(quad)
		quads := grid.GetRegion(Vector3f{-10, -10, -10}, Vector3f{10, 10, 10})
		require.True(t, 1 == len(quads))
	})

	t.Run("GetRegion: Get half the session", func(t *testing.T) {
		quad := Quad{
			Center:  Vector3f{2, 0, 2},
			Extents: Vector3f{1, 0, 1},
			Normal:  Vector3f{0, 1, 0},
		}
		grid.InsertQuad(quad)
		quads := grid.GetRegion(Vector3f{0, 0, 0}, Vector3f{10, 10, 10})
		require.True(t, 1 == len(quads))
	})
}

func TestGridQuadMerging(t *testing.T) {
	grid := NewRegularGrid(1, 1, 1)

	t.Run("QuadMerging: Setup", func(t *testing.T) {
		quad := Quad{
			Center:  Vector3f{0, 0, 0},
			Extents: Vector3f{2, 0, 2},
			Normal:  Vector3f{0, 1, 0},
		}
		grid.InsertQuad(quad)
		require.True(t, grid.PlaneCount == 1)
		require.True(t, grid.MergeCount == 0)
		require.True(t, 1 == len(grid.Grid[0][4]))
	})

	t.Run("QuadMerging: Test merging", func(t *testing.T) {
		quad := Quad{
			Center:  Vector3f{0, 0, 0},
			Extents: Vector3f{0.001, 0, 0.001},
			Normal:  Vector3f{0, 1, 0},
		}
		grid.InsertQuad(quad)
		require.True(t, grid.PlaneCount == 1)
		require.True(t, grid.MergeCount == 1)

		// check the grid shrinked:
		require.True(t, 0 == len(grid.Grid[0][4]))
	})
}
