package dagaz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEqualWithEpsilon(t *testing.T) {
	require.True(t, EqualWithEpsilon(0.1, 0.2, 0.11))
}

func TestDot(t *testing.T) {
	xAxis := Vector3f{1, 0, 0}
	yAxis := Vector3f{0, 1, 0}

	require.Equal(t, (float32)(0), xAxis.Dot(yAxis))
}

func TestCross(t *testing.T) {
	xAxis := Vector3f{1, 0, 0}
	yAxis := Vector3f{0, 1, 0}
	zAxis := Vector3f{0, 0, 1}

	require.True(t, zAxis.Equal(Cross(xAxis, yAxis)))
}

func TestIntersectQuad(t *testing.T) {
	ray := Ray{
		From: Vector3f{0, 10, 0},
		To:   Vector3f{0, -10, 0},
	}
	quad := Quad{
		Center:  Vector3f{0, 0, 0},
		Extents: Vector3f{1, 0, 1},
		Normal:  Vector3f{0, 1, 0},
	}

	hit, _ := IntersectQuad(ray, quad)

	require.True(t, hit)
}

func TestVectorClass(t *testing.T) {
	zeroVector := Vector3f{0, 0, 0}
	oneVector := Vector3f{1, 1, 1}

	require.True(t, zeroVector.Equal(Vector3f{0, 0, 0}))
	require.True(t, oneVector.EqualWithEpsilon(Vector3f{0.9, 1.1, 1}, 0.11))
	require.True(t, oneVector.GreaterThan(zeroVector))
	require.True(t, oneVector.GreaterOrEqualThan(oneVector))
	require.True(t, zeroVector.LesserThan(oneVector))
	require.True(t, zeroVector.LesserOrEqualThan(zeroVector))

	require.True(t, oneVector.Equal(Add(zeroVector, oneVector)))
	require.True(t, oneVector.Equal(Sub(oneVector, zeroVector)))
	require.True(t, zeroVector.Equal(Mul(oneVector, 0)))

	l1Vector := Vector3f{1, 0, 0}
	require.True(t, 1 == l1Vector.Length())

	normalizedOneVector := Normalized(oneVector)
	require.True(t, EqualWithEpsilon((float32)(normalizedOneVector.Length()), 1, 0.001))

	oneVector.NormalizeInPlace()
	require.True(t, EqualWithEpsilon((float32)(oneVector.Length()), 1, 0.001))
}

func TestDoHorizontalPlanesOverlap(t *testing.T) {
	quad := Quad{
		Center:  Vector3f{0, 0, 0},
		Extents: Vector3f{1, 0, 1},
		Normal:  Vector3f{0, 1, 0},
	}

	require.True(t, doHorizontalPlanesOverlap(quad, quad))

	anotherQuad := Quad{
		Center:  Vector3f{10, 0, 0},
		Extents: Vector3f{1, 0, 1},
		Normal:  Vector3f{0, 1, 0},
	}

	require.False(t, doHorizontalPlanesOverlap(quad, anotherQuad))
}

func TestCalculateNormal(t *testing.T) {
	center := Vector3f{0, 0, 0}
	extents := Vector3f{1, 0, 1}
	normal := calculateNormal(center, extents)

	upVector := Vector3f{0, 1, 0}
	require.True(t, upVector.EqualWithEpsilon(normal, 0.0001))
}
