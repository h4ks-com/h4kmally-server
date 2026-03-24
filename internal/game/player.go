package game

import (
	"math"
	"math/rand/v2"
	"sync"
)

// Player represents a connected client.
type Player struct {
	ID     uint32 // unique player ID
	Name   string
	Skin   string
	Effect string // border effect (e.g. "neon", "prismatic", "starfield", "lightning")
	Clan   string

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

	// Direction lock: when locked, movement uses fixedDir instead of mouse
	DirectionLocked bool
	LockedDirX      float64 // normalized
	LockedDirY      float64 // normalized

	// Freeze position: when frozen, cells don't move at all (X key)
	Frozen bool

	// State
	Alive     bool
	Score     float64 // total mass
	SpawnProt int     // ticks of spawn protection remaining

	// ── Session stats (reset on spawn, used for daily goal tracking) ──
	SessionKills     int     // player cells eaten this life
	SessionSplits    int     // times split was pressed this life
	SessionMassEject float64 // total mass ejected this life
	SessionPeakMass  float64 // highest mass reached this life (no-split tracked separately)
	SessionNoSplit   bool    // true if player has not split this life
	SessionVirusHits int     // viruses shot into players (attributed via eject owner)
	SessionRevenge   int     // revenge kills this session
	SessionGames     int     // games played this session (across lives)

	// Revenge tracking: maps playerID → tick when they last ate one of our cells
	RevengeWindow map[uint32]uint64

	// ── Active powerup state ──
	PowerupInventory map[string]int // type → charges (multiple powerups)
	SpeedBoostTicks  int            // ticks remaining for active speed boost
	GhostModeTicks   int            // ticks remaining for ghost mode
	MassMagnetTicks  int            // ticks remaining for mass magnet

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
func NewPlayer(name, skin, effect string) *Player {
	return &Player{
		ID:     nextPlayerID(),
		Name:   name,
		Skin:   skin,
		Effect: effect,
		Color: [3]uint8{
			uint8(rand.IntN(200) + 55),
			uint8(rand.IntN(200) + 55),
			uint8(rand.IntN(200) + 55),
		},
		Cells:          make([]*Cell, 0, 16),
		SessionNoSplit: true,
		RevengeWindow:  make(map[uint32]uint64),
	}
}

// ResetSessionStats resets per-life stats (called on spawn).
func (p *Player) ResetSessionStats() {
	p.SessionKills = 0
	p.SessionSplits = 0
	p.SessionMassEject = 0
	p.SessionPeakMass = 0
	p.SessionNoSplit = true
	p.SessionVirusHits = 0
	p.SessionRevenge = 0
	p.RevengeWindow = make(map[uint32]uint64)
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

// SetDirectionLock locks or unlocks the player's movement direction.
// When locking, computes the current heading from center-of-mass toward mouse.
func (p *Player) SetDirectionLock(lock bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lock && !p.DirectionLocked {
		// Compute current heading from center of mass toward mouse
		cx, cy := 0.0, 0.0
		totalMass := 0.0
		for _, c := range p.Cells {
			m := c.Mass()
			cx += c.X * m
			cy += c.Y * m
			totalMass += m
		}
		if totalMass > 0 {
			cx /= totalMass
			cy /= totalMass
		}
		dx := p.MouseX - cx
		dy := p.MouseY - cy
		dist := math.Sqrt(dx*dx + dy*dy)
		if dist > 1 {
			p.LockedDirX = dx / dist
			p.LockedDirY = dy / dist
		} else {
			p.LockedDirX = 1
			p.LockedDirY = 0
		}
		p.DirectionLocked = true
	} else if !lock {
		p.DirectionLocked = false
	}
}

// SetFrozen freezes or unfreezes the player's cell positions.
// When frozen, cells stop all movement (mouse-tracking is skipped).
func (p *Player) SetFrozen(freeze bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Frozen = freeze
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
// OgarII canonical formula: 88 * size^(-0.4396754) per tick at 25Hz.
// We factor it as 2.2 * size^(-0.4396754) * 40 (times MoveSpeed) in the engine.
func SpeedForSize(size float64) float64 {
	return 2.2 * math.Pow(size, -0.4396754)
}

// Viewport represents a rectangular view area.
type Viewport struct {
	Left, Top, Right, Bottom float64
}

// ViewportForPlayer computes the visible world rectangle for a player.
// Uses OgarII zoom formula: scale = min(64/totalSize, 1)^0.4
// The base viewport is 1920×1080 (reference resolution) scaled by 1/zoom.
// We add a generous margin (1.5×) so cells entering/leaving view don't pop.
func ViewportForPlayer(p *Player, mapW, mapH float64) Viewport {
	if p == nil || !p.Alive || len(p.Cells) == 0 {
		// Not alive — show entire map
		return Viewport{Left: -mapW, Top: -mapH, Right: mapW, Bottom: mapH}
	}

	cx, cy := p.Center()

	// Sum of all cell radii (OgarII style)
	var totalSize float64
	for _, c := range p.Cells {
		totalSize += c.Size
	}

	return viewportFromSize(cx, cy, totalSize)
}

// ViewportForMultibox computes a viewport centered on the active player,
// sized by the combined size of both primary and multi players.
func ViewportForMultibox(active *Player, multi *Player, mapW, mapH float64) Viewport {
	if active == nil || !active.Alive || len(active.Cells) == 0 {
		return Viewport{Left: -mapW, Top: -mapH, Right: mapW, Bottom: mapH}
	}

	cx, cy := active.Center()

	var totalSize float64
	for _, c := range active.Cells {
		totalSize += c.Size
	}
	if multi != nil && multi.Alive {
		for _, c := range multi.Cells {
			totalSize += c.Size
		}
	}

	return viewportFromSize(cx, cy, totalSize)
}

func viewportFromSize(cx, cy, totalSize float64) Viewport {
	// OgarII zoom formula: scale = min(64 / totalSize, 1) ^ 0.4
	if totalSize < 1 {
		totalSize = 1
	}
	ratio := 64.0 / totalSize
	if ratio > 1.0 {
		ratio = 1.0
	}
	zoom := math.Pow(ratio, 0.4)
	if zoom < 0.01 {
		zoom = 0.01
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
// Uses OgarII's playerRoamViewScale (0.4) for free-roam spectating.
func ViewportForSpectator(cx, cy, mapW, mapH float64) Viewport {
	zoom := 0.4 // OgarII playerRoamViewScale
	halfW := (1920.0 / 2.0) / zoom * 1.5
	halfH := (1080.0 / 2.0) / zoom * 1.5
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
