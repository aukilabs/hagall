package dagaz

import (
	"math"
)

// Regular Grid Spatial Partition
//
// An uniformely sub-divided grid implementing the spatial_partition interface.
// The particularities are:
//  - the grid has a resolution that defines how large a cell is. For example,
//    a resolution of 1 will make each cell hold a 1x1 meter subdivision of session.
//    While a resolution of 100 will make each cell hold a 100x100 meter subdivision of session.
//  - Because we are currently only holding horizontal planes, this becomes a 2D partition...

const MERGE_EPSILON = (float32)(0.6)

type RegularGrid struct {
	Resolution uint
	PlaneCount uint32
	MergeCount uint32
	Min        Vector3f
	Max        Vector3f
	Grid       [][][]*Quad
}

func NewRegularGrid(numCols uint, numRows uint, resolution uint) *RegularGrid {
	if numCols == 0 {
		numCols = 1
	}
	if numRows == 0 {
		numRows = 1
	}
	if resolution == 0 {
		resolution = 1
	}

	result := &RegularGrid{
		Resolution: resolution,
		PlaneCount: 0,
		MergeCount: 0,
		Min:        Vector3f{0, 0, 0},
		Max:        Vector3f{(float32)(resolution), 0, (float32)(resolution)},
		Grid:       [][][]*Quad{},
	}

	result.Grid = make([][][]*Quad, numRows)
	for i := 0; i < (int)(numRows); i++ {
		result.Grid[i] = make([][]*Quad, numCols)
	}

	return result
}

func (grid *RegularGrid) InsertQuad(q Quad) {
	minPoint := Sub(q.Center, q.Extents)
	maxPoint := Add(q.Center, q.Extents)

	// fit the min & max:
	grid.ExpandToFitPoint(&minPoint)
	grid.ExpandToFitPoint(&maxPoint)

	// merging loop:
	// - Find all planes that are occupying the insertion cells: (NOTE: This might not be sufficient because we might be
	//   at the edge of another quad that could be a merge candidate but it is not part of the cells I'm going to occupy.)
	//  1. find closest plane (in y)
	//  2. if needs merging?
	//   3. merge into that one
	//   4. make this the quad we are merging and go to 1
	quadToMerge := &q
	for {
		// +y:
		upRay := Ray{
			From: quadToMerge.Center,
			To:   Vector3f{quadToMerge.Center.x, quadToMerge.Center.y + (MERGE_EPSILON + 1.0), quadToMerge.Center.z},
		}
		hitUp, tUp := grid.IntersectQuad(upRay)

		// -y:
		downRay := Ray{
			From: quadToMerge.Center,
			To:   Vector3f{quadToMerge.Center.x, quadToMerge.Center.y - (MERGE_EPSILON + 1.0), quadToMerge.Center.z},
		}
		hitDown, tDown := grid.IntersectQuad(downRay)

		if hitDown == nil && hitUp == nil {
			break
		}

		t := tUp
		hit := hitUp
		if tDown < t {
			t = tDown
			hit = hitDown
		}

		if EqualWithEpsilon(hit.Center.y, quadToMerge.Center.y, (float64)(MERGE_EPSILON)) && doHorizontalPlanesOverlap(*hit, *quadToMerge) {
			grid.mergeQuads(hit, quadToMerge)

			if hit.Center.Equal(quadToMerge.Center) {
				quadToMerge = nil
				break
			}

			quadToMerge = hit
		} else {
			break
		}
	}

	if quadToMerge == &q {
		// case of append:
		minXGridCoord := (uint)(math.Floor((float64)(minPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
		minYGridCoord := (uint)(math.Floor((float64)(minPoint.z-grid.Min.z) / (float64)(grid.Resolution)))
		maxXGridCoord := (uint)(math.Floor((float64)(maxPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
		maxYGridCoord := (uint)(math.Floor((float64)(maxPoint.z-grid.Min.z) / (float64)(grid.Resolution)))

		for i := minYGridCoord; i <= (uint)(math.Min((float64)(maxYGridCoord), (float64)(len(grid.Grid)-1))); i++ {
			for j := minXGridCoord; j <= (uint)(math.Min((float64)(maxXGridCoord), (float64)(len(grid.Grid[i])-1))); j++ {
				grid.Grid[i][j] = append(grid.Grid[i][j], &q)
			}
		}

		grid.PlaneCount++
	}
}

// This is a very specialized quad intersection method to work with our
// 2D uniformely sessiond grid.
func (grid *RegularGrid) IntersectQuad(r Ray) (*Quad, float32) {
	// discard y for a simplified cast:
	newRay := &Ray{
		From: Vector3f{r.From.x, 0, r.From.z},
		To:   Vector3f{r.To.x, 0, r.To.z},
	}
	rayDir := Sub(newRay.To, newRay.From)

	// check for single cell hit to avoid the extra computations:
	if rayDir.Length() == 0 {
		// figure out initial cell from ray origin:
		cellX := (int)(math.Floor((float64)(newRay.From.x-grid.Min.x) / (float64)(grid.Resolution)))
		cellY := (int)(math.Floor((float64)(newRay.From.z-grid.Min.z) / (float64)(grid.Resolution)))

		if cellX < 0 || cellX >= len(grid.Grid[0]) {
			return nil, -1
		}
		if cellY < 0 || cellY >= len(grid.Grid) {
			return nil, -1
		}

		tMin := (float32)(math.Inf(1))
		var resultQuad *Quad
		for i := 0; i < len(grid.Grid[cellY][cellX]); i++ {
			hit, t := IntersectQuad(r, *grid.Grid[cellY][cellX][i])
			if hit && t < tMin {
				tMin = t
				resultQuad = grid.Grid[cellY][cellX][i]
			}
		}
		return resultQuad, tMin
	}

	// Only need to handle y=0 (xz) plane:
	var xt float32
	if newRay.From.x < grid.Min.x {
		xt = grid.Min.x - newRay.From.x
		if xt > rayDir.x {
			return nil, -1 // No intersection because ray does not reach grid
		}
		xt /= rayDir.x
	} else if newRay.From.x > grid.Max.x {
		xt = grid.Max.x - newRay.From.x
		if xt < rayDir.x {
			return nil, -1 // No intersection because ray does not enter grid
		}
		xt /= rayDir.x
	}

	var zt float32
	if newRay.From.z < grid.Min.z {
		zt = grid.Min.z - newRay.From.z
		if zt > rayDir.z {
			return nil, -1
		}
		zt /= rayDir.z
	} else if newRay.From.z > grid.Max.z {
		zt = grid.Max.z - newRay.From.z
		if zt < rayDir.z {
			return nil, -1
		}
		zt /= rayDir.z
	}

	// calculate the delta for t for x per cell:
	t_minx := (grid.Min.x - newRay.From.x) / rayDir.x
	t_maxx := (grid.Max.x - newRay.From.x) / rayDir.x
	if t_minx > t_maxx {
		Swap(&t_minx, &t_maxx)
	}
	deltaTX := (t_maxx - t_minx) / (float32)(len(grid.Grid[0]))

	// calculate the delta for t for y per cell:
	t_miny := (grid.Min.z - newRay.From.z) / rayDir.z
	t_maxy := (grid.Max.z - newRay.From.z) / rayDir.z
	if t_miny > t_maxy {
		Swap(&t_miny, &t_maxy)
	}
	deltaTY := (t_maxy - t_miny) / (float32)(len(grid.Grid))

	// select the farthest away intersectiona as this is the intersection with the firt hit cell:
	var t float32 = zt
	if xt > zt {
		t = xt
	}

	// start the Bresenham-like algo:
	for {
		hitPoint := Add(newRay.From, Mul(rayDir, t))

		cellX := (uint)(math.Floor((float64)(hitPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
		cellY := (uint)(math.Floor((float64)(hitPoint.z-grid.Min.z) / (float64)(grid.Resolution)))

		// clamp to bounds
		cellX = (uint)(math.Min((float64)(cellX), (float64)(len(grid.Grid[0])-1)))
		cellX = (uint)(math.Min((float64)(cellY), (float64)(len(grid.Grid)-1)))

		tMin := (float32)(math.Inf(1))
		var resultQuad *Quad
		for i := 0; i < len(grid.Grid[cellY][cellX]); i++ {
			hit, t := IntersectQuad(r, *grid.Grid[cellY][cellX][i])
			if hit && t < tMin {
				tMin = t
				resultQuad = grid.Grid[cellY][cellX][i]
			}
		}

		if resultQuad != nil {
			return resultQuad, tMin
		}

		// pick next t:
		if t+deltaTX < t+deltaTY {
			t += deltaTX
		} else {
			t += deltaTY
		}

		if t > 1 || math.IsInf((float64)(t), 0) || math.IsNaN((float64)(t)) {
			break
		}
	}

	return nil, -1
}

func (grid *RegularGrid) GetRegion(min Vector3f, max Vector3f) []*Quad {
	// clamp input to grid size:
	min = Vector3f{(float32)(math.Max((float64)(min.x), (float64)(grid.Min.x))), 0, (float32)(math.Max((float64)(min.z), (float64)(grid.Min.z)))}
	max = Vector3f{(float32)(math.Min((float64)(max.x), (float64)(grid.Max.x))), 0, (float32)(math.Min((float64)(max.z), (float64)(grid.Max.z)))}

	minXGridCoord := (uint)(math.Floor((float64)(min.x-grid.Min.x) / (float64)(grid.Resolution)))
	minYGridCoord := (uint)(math.Floor((float64)(min.z-grid.Min.z) / (float64)(grid.Resolution)))
	maxXGridCoord := (uint)(math.Floor((float64)(max.x-grid.Min.x) / (float64)(grid.Resolution)))
	maxYGridCoord := (uint)(math.Floor((float64)(max.z-grid.Min.z) / (float64)(grid.Resolution)))

	result := make(map[*Quad]bool, 0)
	for y := minYGridCoord; y < maxYGridCoord; y++ {
		for x := minXGridCoord; x < maxXGridCoord; x++ {
			for k := 0; k < len(grid.Grid[y][x]); k++ {
				result[grid.Grid[y][x][k]] = true
			}
		}
	}

	quads := make([]*Quad, len(result))
	i := 0
	for k := range result {
		quads[i] = k
		i++
	}

	return quads
}

func (grid *RegularGrid) GetDebugInfo() SpatialDebugInfo {

	result := SpatialDebugInfo{}
	result.Resolution = (uint32)(grid.Resolution)
	result.Row_count = (uint32)(len(grid.Grid))
	result.Col_count = (uint32)(len(grid.Grid[0]))
	result.Plane_count = grid.PlaneCount
	result.Merge_count = grid.MergeCount
	result.Min_point = grid.Min
	result.Max_point = grid.Max

	result.Occupancy = make([]uint32, result.Row_count*result.Col_count)
	for y := (uint32)(0); y < result.Row_count; y++ {
		for x := (uint32)(0); x < result.Col_count; x++ {
			result.Occupancy[y*result.Col_count+x] = (uint32)(len(grid.Grid[y][x]))
		}
	}

	return result
}

// NOTE(jhenriques): the cells limits are in the range [0..1[
// meaning, for a resolution of 1, the "unit 1" is in "cell index" 1
func (grid *RegularGrid) ExpandToFitPoint(p *Vector3f) {
	if p.x >= grid.Min.x && p.z >= grid.Min.z && p.x < grid.Max.x && p.z < grid.Max.z {
		return
	}

	// xCount and yCount are the amount of new cells to add:
	var xCount int
	if p.x >= grid.Min.x && p.x < grid.Max.x {
		xCount = 0 // We have enough resolution on x
	} else {
		if p.x < grid.Min.x {
			xCount = (int)(math.Abs(math.Floor((float64)(p.x - grid.Min.x))))
		} else {
			xCount = (int)(math.Floor(math.Abs((float64)(p.x-grid.Max.x))) + 1)
		}
	}

	var yCount int
	if p.z >= grid.Min.z && p.z < grid.Max.z {
		yCount = 0 // We have enough resolution on y
	} else {
		if p.z < grid.Min.z {
			yCount = (int)(math.Abs(math.Floor((float64)(p.z - grid.Min.z))))
		} else {
			yCount = (int)(math.Floor(math.Abs((float64)(p.z-grid.Max.z))) + 1)
		}
	}

	xCount = (int)(math.Ceil((float64)(xCount) / (float64)(grid.Resolution)))
	yCount = (int)(math.Ceil((float64)(yCount) / (float64)(grid.Resolution)))

	curColCount := len(grid.Grid)
	curRowCount := len(grid.Grid[0])

	// Add rows:
	if p.x < grid.Min.x {
		// ++row:
		for i := 0; i < curColCount; i++ {
			grid.Grid[i] = append(make([][]*Quad, xCount), grid.Grid[i]...)
		}

		grid.Min.x = grid.Min.x - (float32)((xCount * (int)(grid.Resolution)))

	} else {
		// row++:
		for i := 0; i < curColCount; i++ {
			grid.Grid[i] = append(grid.Grid[i], make([][]*Quad, xCount)...)
		}

		grid.Max.x = grid.Max.x + (float32)((xCount * (int)(grid.Resolution)))
	}

	// Add columns:
	if p.z < grid.Min.z {
		// ++col:
		grid.Grid = append(make([][][]*Quad, yCount), grid.Grid...)
		for i := 0; i < yCount; i++ {
			grid.Grid[i] = make([][]*Quad, xCount+curRowCount)
		}

		grid.Min.z = grid.Min.z - (float32)((yCount * (int)(grid.Resolution)))

	} else {
		// col++:
		grid.Grid = append(grid.Grid, make([][][]*Quad, yCount)...)
		for i := curColCount; i < curColCount+yCount; i++ {
			grid.Grid[i] = make([][]*Quad, xCount+curRowCount)
		}

		grid.Max.z = grid.Max.z + (float32)((yCount * (int)(grid.Resolution)))
	}
}

func (grid *RegularGrid) removeQuadFromCell(toRemove *Quad, x uint, y uint) {
	contains, index := arrayContains(grid.Grid[y][x], toRemove)
	if contains {
		grid.Grid[y][x][index] = grid.Grid[y][x][len(grid.Grid[y][x])-1]
		grid.Grid[y][x] = grid.Grid[y][x][:len(grid.Grid[y][x])-1]
	}
}

func (grid *RegularGrid) mergeQuads(existingQuad *Quad, newQuad *Quad) {

	minPoint := Sub(existingQuad.Center, existingQuad.Extents)
	maxPoint := Add(existingQuad.Center, existingQuad.Extents)
	minXGridCoord0 := (uint)(math.Floor((float64)(minPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
	minYGridCoord0 := (uint)(math.Floor((float64)(minPoint.z-grid.Min.z) / (float64)(grid.Resolution)))
	maxXGridCoord0 := (uint)(math.Floor((float64)(maxPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
	maxYGridCoord0 := (uint)(math.Floor((float64)(maxPoint.z-grid.Min.z) / (float64)(grid.Resolution)))

	centerDiff := Sub(newQuad.Center, existingQuad.Center)
	extentsDiff := Sub(newQuad.Extents, existingQuad.Extents)
	existingQuad.Center.Add(Mul(centerDiff, 0.2))
	existingQuad.Extents.Add(Mul(extentsDiff, 0.2))

	// calculate the min cell and max cell again:
	minPoint = Sub(existingQuad.Center, existingQuad.Extents)
	maxPoint = Add(existingQuad.Center, existingQuad.Extents)
	minXGridCoord1 := (uint)(math.Floor((float64)(minPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
	minYGridCoord1 := (uint)(math.Floor((float64)(minPoint.z-grid.Min.z) / (float64)(grid.Resolution)))
	maxXGridCoord1 := (uint)(math.Floor((float64)(maxPoint.x-grid.Min.x) / (float64)(grid.Resolution)))
	maxYGridCoord1 := (uint)(math.Floor((float64)(maxPoint.z-grid.Min.z) / (float64)(grid.Resolution)))

	minMinX := minXGridCoord0
	maxMinX := minXGridCoord1
	expandLeftEdge := false
	if minXGridCoord1 < minXGridCoord0 { // expanding
		minMinX = minXGridCoord1
		maxMinX = minXGridCoord0
		expandLeftEdge = true
	}

	minMaxX := maxXGridCoord0
	maxMaxX := maxXGridCoord1
	expandRightEdge := true
	if maxXGridCoord1 < maxXGridCoord0 { // shrink
		minMaxX = maxXGridCoord1
		maxMaxX = maxXGridCoord0
		expandRightEdge = false
	}

	minMinY := minYGridCoord0
	maxMinY := minYGridCoord1
	expandTopEdge := false
	if minYGridCoord1 < minYGridCoord0 { // shrink
		minMinY = minYGridCoord1
		maxMinY = minYGridCoord0
		expandTopEdge = true
	}

	minMaxY := maxYGridCoord0
	maxMaxY := maxYGridCoord1
	expandBottomEdge := true
	if maxYGridCoord1 < maxYGridCoord0 { // expanding
		minMaxY = maxYGridCoord1
		maxMaxY = maxYGridCoord0
		expandBottomEdge = false
	}

	// left edge:
	for y := minMinY; y <= maxMaxY; y++ {
		for x := minMinX; x < maxMinX; x++ {
			if expandLeftEdge {
				grid.Grid[y][x] = append(grid.Grid[y][x], existingQuad)
			} else {
				grid.removeQuadFromCell(existingQuad, x, y)
			}
		}
	}

	// right edge:
	for y := minMinY; y <= maxMaxY; y++ {
		for x := maxMaxX; x > minMaxX; x-- {
			if expandRightEdge {
				grid.Grid[y][x] = append(grid.Grid[y][x], existingQuad)
			} else {
				grid.removeQuadFromCell(existingQuad, x, y)
			}
		}
	}

	// top edge:
	for y := minMinY; y < maxMinY; y++ {
		for x := maxMinX; x <= minMaxX; x++ {
			if expandTopEdge {
				grid.Grid[y][x] = append(grid.Grid[y][x], existingQuad)
			} else {
				grid.removeQuadFromCell(existingQuad, x, y)
			}
		}
	}

	// bottom edge:
	for y := maxMaxY; y > minMaxY; y-- {
		for x := maxMinX; x <= minMaxX; x++ {
			if expandBottomEdge {
				grid.Grid[y][x] = append(grid.Grid[y][x], existingQuad)
			} else {
				grid.removeQuadFromCell(existingQuad, x, y)
			}
		}
	}

	existingQuad.MergeCount++
	grid.MergeCount++
}
