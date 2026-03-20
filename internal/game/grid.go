package game

// SpatialGrid is a uniform grid for fast spatial queries.
// Each cell of the grid stores a list of game cells whose center falls in that cell.
// Large objects may overlap multiple grid cells, so queries expand by the max possible radius.
type SpatialGrid struct {
	cellSize float64 // size of each grid cell
	invCell  float64 // 1/cellSize for fast division
	offsetX  float64 // added to world X before hashing (to make coords non-negative)
	offsetY  float64
	cols     int
	rows     int
	buckets  []gridBucket
}

type gridBucket struct {
	cells []*Cell
}

// NewSpatialGrid creates a grid covering the world from (-hw,-hh) to (hw,hh).
// gridCellSize controls the granularity; 400-800 is typical.
func NewSpatialGrid(halfWidth, halfHeight, gridCellSize float64) *SpatialGrid {
	cols := int((halfWidth*2)/gridCellSize) + 1
	rows := int((halfHeight*2)/gridCellSize) + 1
	g := &SpatialGrid{
		cellSize: gridCellSize,
		invCell:  1.0 / gridCellSize,
		offsetX:  halfWidth,
		offsetY:  halfHeight,
		cols:     cols,
		rows:     rows,
		buckets:  make([]gridBucket, cols*rows),
	}
	return g
}

// Clear removes all entries. Call once per tick before re-inserting.
func (g *SpatialGrid) Clear() {
	for i := range g.buckets {
		g.buckets[i].cells = g.buckets[i].cells[:0]
	}
}

// bucketIndex returns the grid bucket index for a world position.
func (g *SpatialGrid) bucketIndex(x, y float64) int {
	col := int((x + g.offsetX) * g.invCell)
	row := int((y + g.offsetY) * g.invCell)
	if col < 0 {
		col = 0
	} else if col >= g.cols {
		col = g.cols - 1
	}
	if row < 0 {
		row = 0
	} else if row >= g.rows {
		row = g.rows - 1
	}
	return row*g.cols + col
}

// Insert adds a cell to the grid based on its center position.
// Also stores the bucket index on the cell for fast removal.
func (g *SpatialGrid) Insert(c *Cell) {
	idx := g.bucketIndex(c.X, c.Y)
	c.gridIdx = idx
	g.buckets[idx].cells = append(g.buckets[idx].cells, c)
}

// Remove removes a cell from its current grid bucket.
// Uses the stored gridIdx for O(1) bucket lookup, then swaps with last.
func (g *SpatialGrid) Remove(c *Cell) {
	idx := c.gridIdx
	if idx < 0 || idx >= len(g.buckets) {
		return
	}
	bucket := &g.buckets[idx]
	for i, bc := range bucket.cells {
		if bc == c {
			last := len(bucket.cells) - 1
			bucket.cells[i] = bucket.cells[last]
			bucket.cells[last] = nil
			bucket.cells = bucket.cells[:last]
			return
		}
	}
}

// Move removes a cell from its old bucket and inserts into the new one.
// Only does work if the cell actually changed grid buckets.
func (g *SpatialGrid) Move(c *Cell) {
	newIdx := g.bucketIndex(c.X, c.Y)
	if newIdx == c.gridIdx {
		return // still in same bucket, no work needed
	}
	g.Remove(c)
	c.gridIdx = newIdx
	g.buckets[newIdx].cells = append(g.buckets[newIdx].cells, c)
}

// QueryRadius returns all cells within maxDist of (cx, cy).
// This is an approximate check — it returns all cells in grid cells that could
// contain a cell within maxDist. Caller should do fine-grained distance checks.
func (g *SpatialGrid) QueryRadius(cx, cy, maxDist float64) []*Cell {
	minCol := int((cx - maxDist + g.offsetX) * g.invCell)
	maxCol := int((cx + maxDist + g.offsetX) * g.invCell)
	minRow := int((cy - maxDist + g.offsetY) * g.invCell)
	maxRow := int((cy + maxDist + g.offsetY) * g.invCell)

	if minCol < 0 {
		minCol = 0
	}
	if maxCol >= g.cols {
		maxCol = g.cols - 1
	}
	if minRow < 0 {
		minRow = 0
	}
	if maxRow >= g.rows {
		maxRow = g.rows - 1
	}

	var result []*Cell
	for row := minRow; row <= maxRow; row++ {
		base := row * g.cols
		for col := minCol; col <= maxCol; col++ {
			result = append(result, g.buckets[base+col].cells...)
		}
	}
	return result
}

// QueryRect appends all cells whose center is within the given rectangle (inclusive)
// to the provided buffer and returns it. Useful for viewport queries.
// Also expands by margin to catch cells whose body extends into the rect.
// Pass buf[:0] to reuse a previously allocated slice.
func (g *SpatialGrid) QueryRect(buf []*Cell, left, top, right, bottom, margin float64) []*Cell {
	minCol := int((left - margin + g.offsetX) * g.invCell)
	maxCol := int((right + margin + g.offsetX) * g.invCell)
	minRow := int((top - margin + g.offsetY) * g.invCell)
	maxRow := int((bottom + margin + g.offsetY) * g.invCell)

	if minCol < 0 {
		minCol = 0
	}
	if maxCol >= g.cols {
		maxCol = g.cols - 1
	}
	if minRow < 0 {
		minRow = 0
	}
	if maxRow >= g.rows {
		maxRow = g.rows - 1
	}

	for row := minRow; row <= maxRow; row++ {
		base := row * g.cols
		for col := minCol; col <= maxCol; col++ {
			buf = append(buf, g.buckets[base+col].cells...)
		}
	}
	return buf
}
