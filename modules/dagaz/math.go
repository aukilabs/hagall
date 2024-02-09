package dagaz

import (
	"math"

	"github.com/aukilabs/hagall-common/messages/dagazpb"
)

func Swap(a *float32, b *float32) {
	*a, *b = *b, *a
}

func EqualWithEpsilon(a float32, b float32, epsilon float64) bool {
	return math.Abs((float64)(a-b)) <= epsilon
}

func InRangeWithEpsilon(value float32, min float32, max float32, epsilon float32) bool {
	return value+epsilon >= min && value-epsilon <= max
}

type Vector3f struct {
	x float32
	y float32
	z float32
}

func NewVector3f(x, y, z float32) Vector3f {
	return Vector3f{x, y, z}
}

func (v1 Vector3f) EqualWithEpsilon(v2 Vector3f, epsilon float64) bool {
	return math.Abs((float64)(v1.x-v2.x)) <= epsilon &&
		math.Abs((float64)(v1.y-v2.y)) <= epsilon &&
		math.Abs((float64)(v1.z-v2.z)) <= epsilon
}

func (v1 *Vector3f) Equal(v2 Vector3f) bool {
	return v1.x == v2.x && v1.y == v2.y && v1.z == v2.z
}

func (v1 *Vector3f) GreaterOrEqualThan(v2 Vector3f) bool {
	return v1.x >= v2.x && v1.y >= v2.y && v1.z >= v2.z
}

func (v1 *Vector3f) GreaterThan(v2 Vector3f) bool {
	return v1.x > v2.x && v1.y > v2.y && v1.z > v2.z
}

func (v1 *Vector3f) LesserOrEqualThan(v2 Vector3f) bool {
	return v1.x <= v2.x && v1.y <= v2.y && v1.z <= v2.z
}

func (v1 *Vector3f) LesserThan(v2 Vector3f) bool {
	return v1.x < v2.x && v1.y < v2.y && v1.z < v2.z
}

func (v1 *Vector3f) Add(v2 Vector3f) {
	v1.x += v2.x
	v1.y += v2.y
	v1.z += v2.z
}

func Add(a Vector3f, b Vector3f) Vector3f {
	return Vector3f{a.x + b.x, a.y + b.y, a.z + b.z}
}

func Sub(a Vector3f, b Vector3f) Vector3f {
	return Vector3f{a.x - b.x, a.y - b.y, a.z - b.z}
}

func Mul(a Vector3f, s float32) Vector3f {
	return Vector3f{a.x * s, a.y * s, a.z * s}
}

func (a *Vector3f) Length() float64 {
	return math.Sqrt((float64)(a.x*a.x + a.y*a.y + a.z*a.z))
}

func (a *Vector3f) NormalizeInPlace() {
	lenght := (float32)(a.Length())
	if lenght != 0 {
		a.x /= lenght
		a.y /= lenght
		a.z /= lenght
	}
}

func Normalized(a Vector3f) Vector3f {
	lenght := (float32)(a.Length())
	result := a
	if lenght != 0 {
		result.x /= lenght
		result.y /= lenght
		result.z /= lenght
	}
	return result
}

func (a *Vector3f) Dot(b Vector3f) float32 {
	return a.x*b.x + a.y*b.y + a.z*b.z
}

func Cross(a Vector3f, b Vector3f) Vector3f {
	return Vector3f{a.y*b.z - a.z*b.y, a.z*b.x - a.x*b.z, a.x*b.y - a.y*b.x}
}

func NewVector3fFromProtobuf(point *dagazpb.Point) Vector3f {
	return Vector3f{
		x: point.X,
		y: point.Y,
		z: point.Z,
	}
}

func (v *Vector3f) ToProtobuf() *dagazpb.Point {
	return &dagazpb.Point{
		X: v.x,
		Y: v.y,
		Z: v.z,
	}
}

type Quad struct {
	Center  Vector3f
	Extents Vector3f // Half-Extents!

	// implicit
	Normal Vector3f

	MergeCount uint32
}

func NewQuadFromProtobuf(protoQuad *dagazpb.Quad) Quad {
	center := NewVector3fFromProtobuf(protoQuad.Center)
	extents := NewVector3fFromProtobuf(protoQuad.Extents)

	return Quad{
		Center:     center,
		Extents:    extents,
		Normal:     calculateNormal(center, extents),
		MergeCount: protoQuad.MergeCount,
	}
}

func (q *Quad) ToProtobuf() *dagazpb.Quad {
	return &dagazpb.Quad{
		Center:     q.Center.ToProtobuf(),
		Extents:    q.Extents.ToProtobuf(),
		MergeCount: q.MergeCount,
	}
}

func doHorizontalPlanesOverlap(a Quad, b Quad) bool {
	minA := Sub(a.Center, a.Extents)
	maxA := Add(a.Center, a.Extents)
	minB := Sub(b.Center, b.Extents)
	maxB := Add(b.Center, b.Extents)

	if minA.x >= maxB.x {
		return false
	}
	if maxA.x <= minB.x {
		return false
	}
	if minA.z >= maxB.z {
		return false
	}
	if maxA.z <= minB.z {
		return false
	}

	// overlap on both axes -> must overlap
	return true
}

func calculateNormal(c Vector3f, e Vector3f) Vector3f {
	pointA := Add(c, Vector3f{e.x, e.y, 0})
	pointB := Add(c, Vector3f{0, e.y, e.z})
	vectorA := Sub(pointA, c)
	vectorB := Sub(pointB, c)
	normal := Cross(vectorB, vectorA)
	normal.NormalizeInPlace()
	return normal
}

type Ray struct {
	From Vector3f
	To   Vector3f
}

func NewRayFromProtobuf(protoRay *dagazpb.Ray) Ray {
	from := NewVector3fFromProtobuf(protoRay.From)
	to := NewVector3fFromProtobuf(protoRay.To)

	return Ray{
		From: from,
		To:   to,
	}
}

func arrayContains(array []*Quad, quad *Quad) (bool, uint) {
	for q := 0; q < len(array); q++ {
		if quad == array[q] {
			return true, (uint)(q)
		}
	}
	return false, 0
}

func IntersectQuad(r Ray, q Quad) (bool, float32) {
	rayDir := Sub(r.To, r.From)

	denominator := q.Normal.Dot(rayDir)
	if denominator != 0 {
		t := (q.Normal.Dot(q.Center) - q.Normal.Dot(r.From)) / denominator
		if t >= 0 && t <= 1 {
			hitPoint := Add(r.From, Mul(rayDir, t))

			// check hitPoint is in bounds:
			minPoint := Sub(q.Center, q.Extents)
			maxPoint := Add(q.Center, q.Extents)
			if InRangeWithEpsilon(hitPoint.x, minPoint.x, maxPoint.x, 0.0001) &&
				InRangeWithEpsilon(hitPoint.y, minPoint.y, maxPoint.y, 0.0001) &&
				InRangeWithEpsilon(hitPoint.z, minPoint.z, maxPoint.z, 0.0001) {
				return true, t
			}
		}
	}
	return false, -1
}
