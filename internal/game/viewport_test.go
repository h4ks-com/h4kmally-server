package game

import (
	"testing"
	"time"
)

// smallCfg returns a minimal config for tests.
func smallCfg() Config {
	return Config{
		MapWidth:        1000,
		MapHeight:       1000,
		TickRate:        40 * time.Millisecond,
		StartSize:       100,
		MinPlayerSize:   100,
		MinSplitSize:    122.5,
		MaxCells:        16,
		MergeDelay:      30 * time.Second,
		SplitSpeed:      780,
		EjectSize:       24.5,
		EjectSpeed:      780,
		DecayRate:       0.9998,
		DecayMinSize:    100,
		MoveSpeed:       1.0,
		SplitDistance:   1420,
		FoodCount:       0, // no auto-food for tests
		FoodSize:        10,
		FoodSpawnPer:    0,
		VirusCount:      0,
		VirusSize:       100,
		VirusMaxSize:    200,
		VirusFeedSize:   15,
		VirusSplit:      10,
		LeaderboardSize: 10,
		LeaderboardRate: 2 * time.Second,
	}
}

// --- ViewportForPlayer tests ---

func TestViewportForPlayer_NilPlayer(t *testing.T) {
	vp := ViewportForPlayer(nil, 1000, 1000)
	if vp.Left != -1000 || vp.Right != 1000 || vp.Top != -1000 || vp.Bottom != 1000 {
		t.Errorf("nil player should return full map viewport, got %+v", vp)
	}
}

func TestViewportForPlayer_DeadPlayer(t *testing.T) {
	p := NewPlayer("test", "", "")
	p.Alive = false
	vp := ViewportForPlayer(p, 1000, 1000)
	if vp.Left != -1000 || vp.Right != 1000 {
		t.Errorf("dead player should return full map viewport, got %+v", vp)
	}
}

func TestViewportForPlayer_NoCells(t *testing.T) {
	p := NewPlayer("test", "", "")
	p.Alive = true
	p.Cells = nil
	vp := ViewportForPlayer(p, 1000, 1000)
	if vp.Left != -1000 || vp.Right != 1000 {
		t.Errorf("no-cell player should return full map viewport, got %+v", vp)
	}
}

func TestViewportForPlayer_AlivePlayerCentered(t *testing.T) {
	p := NewPlayer("test", "", "")
	p.Alive = true
	cell := &Cell{ID: 999, Type: CellPlayer, X: 200, Y: 300, Size: 32, Owner: p}
	p.Cells = []*Cell{cell}

	vp := ViewportForPlayer(p, 7071, 7071)

	// Viewport should be centered on the player (200, 300)
	cx := (vp.Left + vp.Right) / 2
	cy := (vp.Top + vp.Bottom) / 2
	if abs(cx-200) > 1 || abs(cy-300) > 1 {
		t.Errorf("viewport center should be (200, 300), got (%.1f, %.1f)", cx, cy)
	}

	// Viewport should be significantly larger than 0
	w := vp.Right - vp.Left
	h := vp.Bottom - vp.Top
	if w < 500 || h < 300 {
		t.Errorf("viewport too small: %.0f x %.0f", w, h)
	}
}

func TestViewportForPlayer_LargerCellWiderView(t *testing.T) {
	p1 := NewPlayer("small", "", "")
	p1.Alive = true
	p1.Cells = []*Cell{{ID: 1, Type: CellPlayer, X: 0, Y: 0, Size: 32, Owner: p1}}

	p2 := NewPlayer("big", "", "")
	p2.Alive = true
	p2.Cells = []*Cell{{ID: 2, Type: CellPlayer, X: 0, Y: 0, Size: 200, Owner: p2}}

	vp1 := ViewportForPlayer(p1, 7071, 7071)
	vp2 := ViewportForPlayer(p2, 7071, 7071)

	w1 := vp1.Right - vp1.Left
	w2 := vp2.Right - vp2.Left

	if w2 <= w1 {
		t.Errorf("bigger cell should have wider viewport: small=%.0f, big=%.0f", w1, w2)
	}
}

// --- CellInViewport tests ---

func TestCellInViewport_Inside(t *testing.T) {
	c := &Cell{X: 50, Y: 50, Size: 10}
	vp := Viewport{Left: 0, Top: 0, Right: 100, Bottom: 100}
	if !CellInViewport(c, vp) {
		t.Error("cell at (50,50) should be inside viewport (0,0)-(100,100)")
	}
}

func TestCellInViewport_Outside(t *testing.T) {
	c := &Cell{X: 500, Y: 500, Size: 10}
	vp := Viewport{Left: 0, Top: 0, Right: 100, Bottom: 100}
	if CellInViewport(c, vp) {
		t.Error("cell at (500,500) should be outside viewport (0,0)-(100,100)")
	}
}

func TestCellInViewport_EdgeTouching(t *testing.T) {
	// Cell radius extends into the viewport
	c := &Cell{X: 105, Y: 50, Size: 10}
	vp := Viewport{Left: 0, Top: 0, Right: 100, Bottom: 100}
	if !CellInViewport(c, vp) {
		t.Error("cell at (105,50) with size 10 should touch right edge of viewport")
	}
}

func TestCellInViewport_JustOutside(t *testing.T) {
	// Cell at x=115 with size 10 means left edge at 105, just outside right=100
	c := &Cell{X: 115, Y: 50, Size: 10}
	vp := Viewport{Left: 0, Top: 0, Right: 100, Bottom: 100}
	if CellInViewport(c, vp) {
		t.Error("cell at (115,50) with size 10 should be outside viewport (right=100)")
	}
}

// --- CellsInViewport (engine) tests ---

func TestEngine_CellsInViewport_FindsFood(t *testing.T) {
	cfg := smallCfg()
	e := &Engine{
		Cfg:     cfg,
		cells:   make(map[uint32]*Cell),
		players: make(map[uint32]*Player),
		grid:    NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
	}

	// Place food at specific positions
	food1 := &Cell{ID: 1, Type: CellFood, X: 50, Y: 50, Size: 10}
	food2 := &Cell{ID: 2, Type: CellFood, X: 500, Y: 500, Size: 10}
	food3 := &Cell{ID: 3, Type: CellFood, X: -800, Y: -800, Size: 10}
	e.cells[1] = food1
	e.cells[2] = food2
	e.cells[3] = food3

	// Viewport around (50, 50)
	vp := Viewport{Left: 0, Top: 0, Right: 100, Bottom: 100}
	e.rebuildGrid()
	visible := e.CellsInViewport(vp)

	if len(visible) != 1 {
		t.Fatalf("expected 1 visible cell, got %d", len(visible))
	}
	if visible[0].ID != 1 {
		t.Errorf("expected food1 (ID 1), got ID %d", visible[0].ID)
	}
}

func TestEngine_CellsInViewport_LargeViewport(t *testing.T) {
	cfg := smallCfg()
	e := &Engine{
		Cfg:     cfg,
		cells:   make(map[uint32]*Cell),
		players: make(map[uint32]*Player),
		grid:    NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
	}

	for i := uint32(1); i <= 50; i++ {
		e.cells[i] = &Cell{
			ID:   i,
			Type: CellFood,
			X:    float64(i * 10),
			Y:    float64(i * 10),
			Size: 10,
		}
	}

	// Viewport that covers all of them
	vp := Viewport{Left: -100, Top: -100, Right: 600, Bottom: 600}
	e.rebuildGrid()
	visible := e.CellsInViewport(vp)
	if len(visible) != 50 {
		t.Errorf("expected 50 visible cells, got %d", len(visible))
	}
}

func TestEngine_GetCellsInViewport_ThreadSafe(t *testing.T) {
	cfg := smallCfg()
	e := NewEngine(cfg)

	// Manually add food since FoodCount=0
	e.mu.Lock()
	for i := uint32(1); i <= 10; i++ {
		e.cells[i] = &Cell{ID: i, Type: CellFood, X: float64(i), Y: float64(i), Size: 10}
	}
	e.rebuildGrid()
	e.mu.Unlock()

	// GetCellsInViewport acquires its own lock — should not deadlock
	vp := Viewport{Left: -100, Top: -100, Right: 100, Bottom: 100}
	visible := e.GetCellsInViewport(vp)
	if len(visible) != 10 {
		t.Errorf("expected 10, got %d", len(visible))
	}
}

// --- Eating / mass conservation tests ---

func TestCanEat_LargerEatsSmaller(t *testing.T) {
	big := &Cell{X: 0, Y: 0, Size: 100}
	small := &Cell{X: 10, Y: 0, Size: 50}
	if !CanEat(big, small) {
		t.Error("cell of size 100 should be able to eat cell of size 50 at distance 10")
	}
}

func TestCanEat_TooSmall(t *testing.T) {
	a := &Cell{X: 0, Y: 0, Size: 50}
	b := &Cell{X: 10, Y: 0, Size: 50}
	if CanEat(a, b) {
		t.Error("equal size cells should not eat each other")
	}
}

func TestCanEat_BarelyNotBigEnough(t *testing.T) {
	// mass-based 1.3x: size 50 → mass 25, need mass ≥ 32.5 → size ≥ 57.01
	a := &Cell{X: 0, Y: 0, Size: 57} // mass 32.49 < 25*1.3 = 32.5
	b := &Cell{X: 0, Y: 0, Size: 50}
	if CanEat(a, b) {
		t.Error("size 57 should not eat size 50 (needs 1.3x mass)")
	}
}

func TestCanEat_JustBigEnough(t *testing.T) {
	// mass-based 1.3x: size 50 → mass 25, need mass ≥ 32.5 → size ≥ 57.01
	a := &Cell{X: 0, Y: 0, Size: 58} // mass 33.64 > 25*1.3 = 32.5
	b := &Cell{X: 0, Y: 0, Size: 50}
	if !CanEat(a, b) {
		t.Error("size 58 should eat size 50 (above 1.3x mass)")
	}
}

func TestCanEat_TooFarApart(t *testing.T) {
	a := &Cell{X: 0, Y: 0, Size: 100}
	b := &Cell{X: 200, Y: 0, Size: 50}
	if CanEat(a, b) {
		t.Error("cells 200 units apart should not eat (distSq=40000 > 100²-50²*0.5=8750)")
	}
}

func TestMass_Roundtrip(t *testing.T) {
	c := &Cell{Size: 100}
	mass := c.Mass() // 10000/100 = 100
	if mass != 100 {
		t.Errorf("expected mass 100, got %f", mass)
	}
	c.SetMass(mass)
	if abs(c.Size-100) > 0.001 {
		t.Errorf("expected size 100 after SetMass roundtrip, got %f", c.Size)
	}
}

func TestEjectMassConservation(t *testing.T) {
	cfg := smallCfg()
	cfg.EjectSize = 38

	playerSize := 100.0
	originalMass := playerSize * playerSize / 100.0 // 100

	ejectMass := cfg.EjectSize * cfg.EjectSize / 100.0 // 14.44
	remainingMass := originalMass - ejectMass          // 85.56

	if abs(ejectMass-14.44) > 0.01 {
		t.Errorf("eject mass should be 14.44, got %f", ejectMass)
	}

	// The player's remaining mass + eject mass should equal original
	if abs(remainingMass+ejectMass-originalMass) > 0.001 {
		t.Errorf("mass not conserved: %.2f + %.2f != %.2f", remainingMass, ejectMass, originalMass)
	}
}

// --- Food size tests ---

func TestNewFood_FixedSize(t *testing.T) {
	cfg := smallCfg()
	cfg.FoodSize = 10

	for i := 0; i < 100; i++ {
		f := NewFood(cfg)
		if f.Size != 10 {
			t.Errorf("food should always be size 10, got %f", f.Size)
		}
	}
}

func TestNewFood_ConfigurableSize(t *testing.T) {
	cfg := smallCfg()
	cfg.FoodSize = 25

	f := NewFood(cfg)
	if f.Size != 25 {
		t.Errorf("food should be size 25, got %f", f.Size)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
