package game

import (
	"math"
	"math/rand/v2"
	"sync"
)

// Player represents a connected client.
type Player struct {
	ID   uint32 // unique player ID
	Name string
	Skin string
	Clan string

	Color        [3]uint8
	IsSubscriber bool

	// Mouse target
	MouseX, MouseY float64

	// Cells owned by this player (ordered by size descending)
	Cells []*Cell

	// Input actions queued for next tick
	mu         sync.Mutex
	splitQueue int
	ejectQueue int

	// Eject rate limiting (server-side cap: max 10 ejects/sec at 25Hz = 1 per ~2.5 ticks)
	lastEjectTick uint64

	// State
	Alive     bool
	Score     float64 // total mass
	SpawnProt int     // ticks of spawn protection remaining

	// Connection reference (set externally)
	Conn interface{}
}

var playerIDGen uint32
var playerIDMu sync.Mutex

func nextPlayerID() uint32 {
	playerIDMu.Lock()
	defer playerIDMu.Unlock()
	playerIDGen++
	return playerIDGen
}

// NewPlayer creates a new player with a random color.
func NewPlayer(name, skin string) *Player {
	return &Player{
		ID:   nextPlayerID(),
		Name: name,
		Skin: skin,
		Color: [3]uint8{
			uint8(rand.IntN(200) + 55),
			uint8(rand.IntN(200) + 55),
			uint8(rand.IntN(200) + 55),
		},
		Cells: make([]*Cell, 0, 16),
	}
}

// TotalMass returns the sum of mass across all cells.
func (p *Player) TotalMass() float64 {
	var total float64
	for _, c := range p.Cells {
		total += c.Mass()
	}
	return total
}

// TotalSize returns the sum of size (radius) across all cells.
func (p *Player) TotalSize() float64 {
	var total float64
	for _, c := range p.Cells {
		total += c.Size
	}
	return total
}

// Center returns the mass-weighted center of all cells.
func (p *Player) Center() (float64, float64) {
	if len(p.Cells) == 0 {
		return 0, 0
	}
	var cx, cy, totalMass float64
	for _, c := range p.Cells {
		m := c.Mass()
		cx += c.X * m
		cy += c.Y * m
		totalMass += m
	}
	if totalMass == 0 {
		return p.Cells[0].X, p.Cells[0].Y
	}
	return cx / totalMass, cy / totalMass
}

// LargestCell returns the player's biggest cell.
func (p *Player) LargestCell() *Cell {
	if len(p.Cells) == 0 {
		return nil
	}
	best := p.Cells[0]
	for _, c := range p.Cells[1:] {
		if c.Size > best.Size {
			best = c
		}
	}
	return best
}

// SmallestCell returns the player's smallest cell.
func (p *Player) SmallestCell() *Cell {
	if len(p.Cells) == 0 {
		return nil
	}
	best := p.Cells[0]
	for _, c := range p.Cells[1:] {
		if c.Size < best.Size {
			best = c
		}
	}
	return best
}

// QueueSplit queues a split action.
func (p *Player) QueueSplit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.splitQueue++
}

// QueueEject queues an eject action.
func (p *Player) QueueEject() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ejectQueue++
}

// SetMouse updates the mouse target position.
func (p *Player) SetMouse(x, y float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.MouseX = x
	p.MouseY = y
}

// ConsumeSplit consumes one split from the queue. Returns true if there was one.
func (p *Player) ConsumeSplit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.splitQueue > 0 {
		p.splitQueue--
		return true
	}
	return false
}

// ConsumeEject consumes one eject from the queue. Returns true if there was one.
func (p *Player) ConsumeEject() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ejectQueue > 0 {
		p.ejectQueue--
		return true
	}
	return false
}

// RemoveCell removes a cell from the player's cell list.
func (p *Player) RemoveCell(id uint32) {
	for i, c := range p.Cells {
		if c.ID == id {
			p.Cells = append(p.Cells[:i], p.Cells[i+1:]...)
			return
		}
	}
}

// SpeedForSize returns the movement speed for a given cell size.
func SpeedForSize(size float64) float64 {
	// Agar.io-style speed formula: smaller = faster
	return 2.2 * math.Pow(size, -0.439)
}

// Viewport represents a rectangular view area.
type Viewport struct {
	Left, Top, Right, Bottom float64
}

// ViewportForPlayer computes the visible world rectangle for a player.
// Uses the same zoom formula as the client: zoom = (64/avgSize)^0.4 * 1.2
// The base viewport is 1920×1080 (reference resolution) scaled by 1/zoom.
// We add a generous margin (1.5×) so cells entering/leaving view don't pop.
func ViewportForPlayer(p *Player, mapW, mapH float64) Viewport {
	if p == nil || !p.Alive || len(p.Cells) == 0 {
		// Not alive — show entire map
		return Viewport{Left: -mapW, Top: -mapH, Right: mapW, Bottom: mapH}
	}

	cx, cy := p.Center()

	// Compute equivalent radius from total mass (same as client)
	var totalMass float64
	for _, c := range p.Cells {
		totalMass += c.Size * c.Size / 100.0
	}
	equivRadius := math.Sqrt(math.Max(1, totalMass)) * 10.0
	if equivRadius < 100 {
		equivRadius = 100
	}

	// Proportional zoom: sqrt-based, matches client formula
	zoom := math.Sqrt(100.0 / equivRadius)
	if zoom < 0.02 {
		zoom = 0.02
	}
	if zoom > 2.0 {
		zoom = 2.0
	}

	// Reference viewport at zoom=1 is approximately 1920×1080
	// Actual viewport in world units = screenSize / zoom
	// Add 1.5× margin for smoothness
	halfW := (1920.0 / 2.0) / zoom * 1.5
	halfH := (1080.0 / 2.0) / zoom * 1.5

	return Viewport{
		Left:   cx - halfW,
		Top:    cy - halfH,
		Right:  cx + halfW,
		Bottom: cy + halfH,
	}
}

// ViewportForSpectator computes the visible world rectangle for a spectator at a given position.
// Uses the same viewport size as a starting-size player (zoom = 1.0).
func ViewportForSpectator(cx, cy, mapW, mapH float64) Viewport {
	// Starting player: mass=100, equivRadius=100, zoom=1.0
	halfW := (1920.0 / 2.0) * 1.5
	halfH := (1080.0 / 2.0) * 1.5
	return Viewport{
		Left:   cx - halfW,
		Top:    cy - halfH,
		Right:  cx + halfW,
		Bottom: cy + halfH,
	}
}

// CellInViewport checks if a cell is within (or touching) the viewport.
func CellInViewport(c *Cell, v Viewport) bool {
	return c.X+c.Size >= v.Left &&
		c.X-c.Size <= v.Right &&
		c.Y+c.Size >= v.Top &&
		c.Y-c.Size <= v.Bottom
}
