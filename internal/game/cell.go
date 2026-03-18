package game

import (
	"math"
	"math/rand/v2"
	"sync/atomic"
)

// CellType distinguishes the different entity kinds.
type CellType uint8

const (
	CellFood   CellType = 0
	CellEject  CellType = 1
	CellPlayer CellType = 2
	CellVirus  CellType = 3
)

// cellIDGen is a global atomic counter for unique cell IDs.
var cellIDGen atomic.Uint32

func nextCellID() uint32 {
	for {
		id := cellIDGen.Add(1)
		if id != 0 { // 0 is reserved as sentinel
			return id
		}
	}
}

// Cell represents any entity in the game world.
type Cell struct {
	ID   uint32
	Type CellType

	X, Y float64 // position (center)
	Size float64 // radius

	// Velocity for moving cells (split, eject impulse)
	// In Ogar-style model: VX/VY is the boost velocity that decays each tick.
	VX, VY float64

	// Visual
	R, G, B uint8
	Name    string
	Skin    string
	Clan    string

	// Flags
	IsVirus      bool
	IsPlayer     bool
	IsSubscriber bool

	// Ownership
	Owner *Player // nil for food/viruses

	// Merge timer
	MergeAt int64 // tick number when this cell can merge (0 = immediately)

	// Dirty flags for protocol
	ColorDirty bool
	SkinDirty  bool
	NameDirty  bool

	// Collision restore: ticks remaining where push-apart is suppressed after split.
	// Matches Ogar's collisionRestoreTicks.
	NoPushTicks int

	// Virus: number of eject masses absorbed (triggers split at threshold)
	FeedCount int

	// Track if this cell is new this tick
	Born bool
}

// Mass returns the cell's mass computed as size²/100.
func (c *Cell) Mass() float64 {
	return c.Size * c.Size / 100.0
}

// IsBoosting returns true if this cell still has boost velocity.
func (c *Cell) IsBoosting() bool {
	return c.VX*c.VX+c.VY*c.VY > BoostMinSq
}

// SetMass sets the cell size from a mass value.
func (c *Cell) SetMass(m float64) {
	c.Size = math.Sqrt(m * 100.0)
}

// OverlapDist returns the distance between two cells minus overlap threshold.
func OverlapDist(a, b *Cell) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return math.Sqrt(dx*dx+dy*dy) - a.Size
}

// CanEat checks if cell 'a' can eat cell 'b' (Ogar-style).
// Mass requirement: eater must have ≥1.3× the mass of eaten cell.
// Distance check (squared): distSq <= eater.Size² - eaten.Size²*0.5
func CanEat(a, b *Cell) bool {
	if a.Mass() < b.Mass()*1.3 {
		return false
	}
	dx := a.X - b.X
	dy := a.Y - b.Y
	distSq := dx*dx + dy*dy
	return distSq <= a.Size*a.Size-b.Size*b.Size*0.5
}

// CanEatFood checks if a player cell can eat food (Ogar-style).
// Any cell can eat food; food center just needs to be inside the player cell.
func CanEatFood(player, food *Cell) bool {
	dx := player.X - food.X
	dy := player.Y - food.Y
	distSq := dx*dx + dy*dy
	return distSq <= player.Size*player.Size
}

// NewFood creates a food cell at a random position.
func NewFood(cfg Config) *Cell {
	return &Cell{
		ID:         nextCellID(),
		Type:       CellFood,
		X:          rand.Float64()*cfg.MapWidth*2 - cfg.MapWidth,
		Y:          rand.Float64()*cfg.MapHeight*2 - cfg.MapHeight,
		Size:       cfg.FoodSize,
		R:          uint8(rand.IntN(200) + 55),
		G:          uint8(rand.IntN(200) + 55),
		B:          uint8(rand.IntN(200) + 55),
		ColorDirty: true,
		Born:       true,
	}
}

// NewVirus creates a virus cell at a random position.
func NewVirus(cfg Config) *Cell {
	return &Cell{
		ID:         nextCellID(),
		Type:       CellVirus,
		X:          rand.Float64()*(cfg.MapWidth*2-400) - cfg.MapWidth + 200,
		Y:          rand.Float64()*(cfg.MapHeight*2-400) - cfg.MapHeight + 200,
		Size:       cfg.VirusSize,
		R:          0,
		G:          255,
		B:          0,
		IsVirus:    true,
		ColorDirty: true,
		Born:       true,
	}
}

// NewPlayerCell creates a new player cell.
func NewPlayerCell(owner *Player, x, y, size float64) *Cell {
	return &Cell{
		ID:           nextCellID(),
		Type:         CellPlayer,
		X:            x,
		Y:            y,
		Size:         size,
		R:            owner.Color[0],
		G:            owner.Color[1],
		B:            owner.Color[2],
		Name:         owner.Name,
		Skin:         owner.Skin,
		IsPlayer:     true,
		IsSubscriber: owner.IsSubscriber,
		Owner:        owner,
		ColorDirty:   true,
		SkinDirty:    true,
		NameDirty:    true,
		Born:         true,
	}
}

// NewEject creates an ejected mass cell.
func NewEject(x, y, vx, vy float64, r, g, b uint8, size float64) *Cell {
	return &Cell{
		ID:         nextCellID(),
		Type:       CellEject,
		X:          x,
		Y:          y,
		Size:       size,
		VX:         vx,
		VY:         vy,
		R:          r,
		G:          g,
		B:          b,
		ColorDirty: true,
		Born:       true,
	}
}
