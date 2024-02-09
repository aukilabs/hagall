package dagaz

type SpatialDebugInfo struct {
	Resolution  uint32
	Row_count   uint32
	Col_count   uint32
	Plane_count uint32
	Merge_count uint32
	Min_point   Vector3f
	Max_point   Vector3f
	Occupancy   []uint32
}

type SpatialPartition interface {
	InsertQuad(q Quad)
	IntersectQuad(r Ray) (*Quad, float32)
	GetRegion(min Vector3f, max Vector3f) []*Quad

	// debug stuff:
	GetDebugInfo() SpatialDebugInfo
}
