package game

import (
	"testing"
)

// TestBroadcastScenario_PlayerMovesIntoFoodArea simulates the core problem:
// food exists at position (500,500). Player starts at (0,0). Player moves to (500,500).
// The viewport around (500,500) should discover the food.
func TestBroadcastScenario_PlayerMovesIntoFoodArea(t *testing.T) {
	cfg := smallCfg()
	e := &Engine{
		Cfg:     cfg,
		cells:   make(map[uint32]*Cell),
		players: make(map[uint32]*Player),
		grid:    NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
	}

	// Place food at (500, 500) — far from the player's starting position
	food := &Cell{ID: 100, Type: CellFood, X: 500, Y: 500, Size: 10, R: 255, G: 0, B: 0}
	e.cells[100] = food

	// Create player at (0, 0)
	p := NewPlayer("mover", "", "")
	p.Alive = true
	playerCell := &Cell{ID: 200, Type: CellPlayer, X: 0, Y: 0, Size: 32, Owner: p, IsPlayer: true}
	p.Cells = []*Cell{playerCell}
	e.cells[200] = playerCell
	e.players[p.ID] = p

	// knownCells simulates what the client has been told about
	knownCells := make(map[uint32]bool)
	knownCells[200] = true // client knows about own cell

	// Step 1: viewport at (0,0) — food at (500,500) should NOT be visible
	vp1 := ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
	e.rebuildGrid()
	visible1 := e.CellsInViewport(vp1)

	foundFood := false
	for _, c := range visible1 {
		if c.ID == 100 {
			foundFood = true
		}
	}

	// The viewport at starting size 32 shouldn't reach (500,500)
	vpWidth := vp1.Right - vp1.Left
	t.Logf("Viewport at (0,0) size 32: width=%.0f, left=%.0f, right=%.0f", vpWidth, vp1.Left, vp1.Right)
	if foundFood {
		t.Log("Note: food at (500,500) IS in viewport from (0,0) — viewport is very wide")
	}

	// Step 2: Move player to (500, 500)
	playerCell.X = 500
	playerCell.Y = 500

	vp2 := ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
	e.rebuildGrid()
	visible2 := e.CellsInViewport(vp2)

	// Build visibleSet (what the server's Broadcast does)
	visibleSet := make(map[uint32]bool)
	for _, c := range visible2 {
		visibleSet[c.ID] = true
	}

	// The food at (500,500) MUST be in the viewport now
	if !visibleSet[100] {
		t.Fatal("food at (500,500) should be visible when player is at (500,500)")
	}

	// Simulate what Broadcast does: find cells in viewport not in knownCells
	var newCells []*Cell
	for _, c := range visible2 {
		if !knownCells[c.ID] {
			newCells = append(newCells, c)
			knownCells[c.ID] = true
		}
	}

	// The food should be discovered as a NEW cell
	foundNewFood := false
	for _, c := range newCells {
		if c.ID == 100 {
			foundNewFood = true
		}
	}
	if !foundNewFood {
		t.Error("food at (500,500) should be discovered as NEW when player moves there")
	}
}

// TestBroadcastScenario_FoodLeavesViewport tests that food is removed
// from knownCells when the player moves away.
func TestBroadcastScenario_FoodLeavesViewport(t *testing.T) {
	cfg := smallCfg()
	e := &Engine{
		Cfg:     cfg,
		cells:   make(map[uint32]*Cell),
		players: make(map[uint32]*Player),
		grid:    NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
	}

	// Place food near origin
	food := &Cell{ID: 100, Type: CellFood, X: 10, Y: 10, Size: 10}
	e.cells[100] = food

	// Player starts at (0, 0) — food is in viewport
	p := NewPlayer("mover", "", "")
	p.Alive = true
	playerCell := &Cell{ID: 200, Type: CellPlayer, X: 0, Y: 0, Size: 32, Owner: p}
	p.Cells = []*Cell{playerCell}
	e.cells[200] = playerCell

	knownCells := make(map[uint32]bool)
	knownCells[100] = true
	knownCells[200] = true

	// Move player far away
	playerCell.X = 900
	playerCell.Y = 900

	vp := ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
	e.rebuildGrid()
	visible := e.CellsInViewport(vp)

	visibleSet := make(map[uint32]bool)
	for _, c := range visible {
		visibleSet[c.ID] = true
	}

	// Simulate removal detection (what Broadcast does)
	var removedIDs []uint32
	for id := range knownCells {
		if !visibleSet[id] {
			removedIDs = append(removedIDs, id)
			delete(knownCells, id)
		}
	}

	// Food should have been removed
	foodRemoved := false
	for _, id := range removedIDs {
		if id == 100 {
			foodRemoved = true
		}
	}
	if !foodRemoved {
		t.Error("food at (10,10) should be removed when player moves to (900,900)")
	}
}

// TestBroadcastScenario_StaticFoodDiscovery is the exact scenario that was broken:
// food spawned many ticks ago (Born=false, not in updated list) must still be
// discovered when a player's viewport moves over it.
func TestBroadcastScenario_StaticFoodDiscovery(t *testing.T) {
	cfg := smallCfg()
	e := &Engine{
		Cfg:     cfg,
		cells:   make(map[uint32]*Cell),
		players: make(map[uint32]*Player),
		grid:    NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
	}

	// 100 food pellets scattered across the map, all with Born=false
	// (simulating food spawned many ticks ago)
	for i := uint32(1); i <= 100; i++ {
		x := float64(i)*20 - 1000
		y := float64(i)*20 - 1000
		e.cells[i] = &Cell{
			ID: i, Type: CellFood, X: x, Y: y, Size: 10,
			R: 255, G: 128, B: 0,
			Born: false, // NOT new this tick
		}
	}

	// Player at origin with size 32
	p := NewPlayer("test", "", "")
	p.Alive = true
	pc := &Cell{ID: 999, Type: CellPlayer, X: 0, Y: 0, Size: 32, Owner: p}
	p.Cells = []*Cell{pc}
	e.cells[999] = pc

	// Simulate initial spawn: client learns about all visible cells
	knownCells := make(map[uint32]bool)
	vp := ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
	e.rebuildGrid()
	visible := e.CellsInViewport(vp)
	for _, c := range visible {
		knownCells[c.ID] = true
	}
	initialKnown := len(knownCells)
	t.Logf("After spawn at (0,0): %d cells known", initialKnown)

	// Now simulate several ticks of movement. The updated list is EMPTY
	// (no cells born or moved), but player moves each tick.
	// The key test: GetCellsInViewport must find food the client doesn't know about.
	positions := [][2]float64{
		{200, 200},
		{400, 400},
		{600, 600},
		{800, 800},
	}

	for _, pos := range positions {
		pc.X = pos[0]
		pc.Y = pos[1]

		vp = ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
		e.rebuildGrid()
		visible = e.CellsInViewport(vp)

		visibleSet := make(map[uint32]bool)
		for _, c := range visible {
			visibleSet[c.ID] = true
		}

		// Discover new cells
		newCount := 0
		for _, c := range visible {
			if !knownCells[c.ID] {
				newCount++
				knownCells[c.ID] = true
			}
		}

		// Remove cells that left viewport
		removedCount := 0
		for id := range knownCells {
			if !visibleSet[id] {
				removedCount++
				delete(knownCells, id)
			}
		}

		t.Logf("At (%.0f, %.0f): visible=%d, new=%d, removed=%d, total_known=%d",
			pos[0], pos[1], len(visible), newCount, removedCount, len(knownCells))
	}

	// After moving far from origin, we should have discovered food from new areas
	// and removed food from old areas. Total known should have changed.
	if len(knownCells) == initialKnown {
		t.Log("Warning: known cells didn't change — viewport may be too large for this map")
	}

	// The critical assertion: GetCellsInViewport MUST return static food
	// (Born=false) that's in the viewport
	pc.X = -900
	pc.Y = -900
	vp = ViewportForPlayer(p, cfg.MapWidth, cfg.MapHeight)
	e.rebuildGrid()
	visible = e.CellsInViewport(vp)

	hasStaticFood := false
	for _, c := range visible {
		if c.Type == CellFood && !c.Born {
			hasStaticFood = true
			break
		}
	}
	if !hasStaticFood && len(visible) > 1 {
		t.Error("CellsInViewport should return static (Born=false) food cells")
	}
}
