package game

import (
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// EatEvent records one cell eating another (for protocol).
type EatEvent struct {
	EaterID uint32
	EatenID uint32
}

// Engine is the core game simulation.
type Engine struct {
	Cfg Config

	mu      sync.RWMutex
	cells   map[uint32]*Cell // all cells by ID
	players map[uint32]*Player
	Grid    *SpatialGrid // spatial hash for fast queries (exported for broadcast)

	// Running counters (avoid O(N) scans)
	foodCount  int
	virusCount int

	// Separate tracking of non-food cells (players, ejects, viruses)
	// so we can skip 2000+ food in moveAllCells and collision scans.
	movable []*Cell // rebuilt each tick from non-food cells

	// Pre-allocated collision buffers (reused each tick)
	active   []*Cell
	toRemove map[uint32]bool

	// Born cells this tick (avoids scanning all 2000+ cells)
	bornThisTick []*Cell

	// Per-tick diff for protocol broadcast
	tickNum uint64
	updated []*Cell    // cells that were added or moved this tick
	eaten   []EatEvent // eat events this tick
	removed []uint32   // cell IDs removed this tick

	// Queued player removals (processed at start of next Tick to avoid
	// grid races between RemovePlayer and concurrent Broadcast reads).
	pendingRemovals []uint32

	// Leaderboard
	Leaderboard  []LeaderEntry
	lastLBUpdate time.Time
}

// LeaderEntry is a leaderboard row.
type LeaderEntry struct {
	Name         string
	Score        float64
	IsMe         bool // set per-player during broadcast
	IsSubscriber bool
}

// NewEngine creates a new game engine.
func NewEngine(cfg Config) *Engine {
	e := &Engine{
		Cfg:      cfg,
		cells:    make(map[uint32]*Cell, 4096),
		movable:  make([]*Cell, 0, 256),
		players:  make(map[uint32]*Player, 64),
		Grid:     NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
		active:   make([]*Cell, 0, 256),
		toRemove: make(map[uint32]bool, 64),
	}
	e.spawnInitialFood()
	e.spawnInitialViruses()
	return e
}

// addCell registers a cell in the engine and inserts it into the spatial grid.
// Must be called under write lock.
func (e *Engine) addCell(c *Cell) {
	e.cells[c.ID] = c
	e.Grid.Insert(c)
	if c.Born {
		e.bornThisTick = append(e.bornThisTick, c)
	}
}

func (e *Engine) spawnInitialFood() {
	for i := 0; i < e.Cfg.FoodCount; i++ {
		c := NewFood(e.Cfg)
		e.cells[c.ID] = c
		e.Grid.Insert(c)
	}
	e.foodCount = e.Cfg.FoodCount
}

func (e *Engine) spawnInitialViruses() {
	for i := 0; i < e.Cfg.VirusCount; i++ {
		c := NewVirus(e.Cfg)
		e.cells[c.ID] = c
		e.Grid.Insert(c)
	}
	e.virusCount = e.Cfg.VirusCount
}

// AddPlayer adds a player to the game (does not spawn cells yet).
func (e *Engine) AddPlayer(p *Player) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.players[p.ID] = p
}

// RemovePlayer removes a player and all their cells immediately.
// MUST only be called while the engine lock is already held (e.g. from within Tick).
// External callers (WS handlers) should use QueueRemovePlayer instead to avoid
// grid data races with concurrent Broadcast reads.
func (e *Engine) removePlayerLocked(id uint32) {
	p, ok := e.players[id]
	if !ok {
		return
	}
	// Remove all cells
	for _, c := range p.Cells {
		e.Grid.Remove(c)
		e.removed = append(e.removed, c.ID)
		delete(e.cells, c.ID)
	}
	p.Cells = nil
	p.Alive = false
	delete(e.players, id)
}

// RemovePlayer acquires the lock and removes a player immediately.
// Prefer QueueRemovePlayer from WS handlers to avoid grid races with Broadcast.
func (e *Engine) RemovePlayer(id uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removePlayerLocked(id)
}

// QueueRemovePlayer enqueues a player for removal at the start of the next Tick.
// This is safe to call from WS handler goroutines — it avoids the data race where
// RemovePlayer modifies the grid while Broadcast workers concurrently read it.
func (e *Engine) QueueRemovePlayer(id uint32) {
	e.mu.Lock()
	e.pendingRemovals = append(e.pendingRemovals, id)
	e.mu.Unlock()
}

// KillPlayersForBR removes the given players (all cells) under the engine lock.
// Used by Battle Royale to eliminate players whose cells fell below the kill threshold.
// Unlike QueueRemovePlayer, this does NOT delete the player from e.players — the
// player stays in the map (with Alive=false) so BR and leaderboard can still see them.
func (e *Engine) KillPlayersForBR(ids []uint32) {
	if len(ids) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		p, ok := e.players[id]
		if !ok || !p.Alive {
			continue
		}
		for _, c := range p.Cells {
			e.Grid.Remove(c)
			e.removed = append(e.removed, c.ID)
			delete(e.cells, c.ID)
		}
		p.Cells = nil
		p.Alive = false
	}
}

// SpawnPlayer spawns a player into the world with one cell.
func (e *Engine) SpawnPlayer(p *Player) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Clean up any existing cells to prevent orphans.
	// This handles the case where a client sends Spawn while still alive
	// (e.g. rapid respawn from external bots). Without this, the old cells
	// remain in e.cells and the grid but are no longer tracked by p.Cells,
	// so removePlayerLocked never cleans them up → permanent ghost cells.
	for _, c := range p.Cells {
		e.Grid.Remove(c)
		e.removed = append(e.removed, c.ID)
		delete(e.cells, c.ID)
	}

	// Find a safe spawn position (away from large cells)
	x, y := e.findSpawnPos()

	cell := NewPlayerCell(p, x, y, e.Cfg.StartSize)
	cell.MergeAt = 0
	e.addCell(cell)

	p.Cells = append(p.Cells[:0], cell)
	p.Alive = true
	p.Score = cell.Mass()
	p.SpawnProt = 75 // ~3 seconds of spawn protection at 25Hz
}

// SpawnPlayerNear spawns a player near a given position with a random offset.
func (e *Engine) SpawnPlayerNear(p *Player, nearX, nearY, offset float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Clean up existing cells (same orphan prevention as SpawnPlayer).
	for _, c := range p.Cells {
		e.Grid.Remove(c)
		e.removed = append(e.removed, c.ID)
		delete(e.cells, c.ID)
	}

	angle := rand.Float64() * 2 * math.Pi
	x := nearX + math.Cos(angle)*offset
	y := nearY + math.Sin(angle)*offset

	// Clamp to map bounds
	w := e.Cfg.MapWidth * 0.95
	h := e.Cfg.MapHeight * 0.95
	if x < -w {
		x = -w
	}
	if x > w {
		x = w
	}
	if y < -h {
		y = -h
	}
	if y > h {
		y = h
	}

	cell := NewPlayerCell(p, x, y, e.Cfg.StartSize)
	cell.MergeAt = 0
	e.addCell(cell)

	p.Cells = append(p.Cells[:0], cell)
	p.Alive = true
	p.Score = cell.Mass()
	p.SpawnProt = 75
}

func (e *Engine) findSpawnPos() (float64, float64) {
	w := e.Cfg.MapWidth * 0.8
	h := e.Cfg.MapHeight * 0.8
	for attempt := 0; attempt < 20; attempt++ {
		x := rand.Float64()*w*2 - w
		y := rand.Float64()*h*2 - h
		safe := true
		// Only check player cells, not all 2000+ cells (food/viruses can't hurt spawns)
		for _, p := range e.players {
			if !p.Alive {
				continue
			}
			for _, c := range p.Cells {
				if c.Size > 100 {
					dx := c.X - x
					dy := c.Y - y
					if dx*dx+dy*dy < c.Size*c.Size*9 { // avoid sqrt: (dist < size*3) → dist² < size²*9
						safe = false
						break
					}
				}
			}
			if !safe {
				break
			}
		}
		if safe {
			return x, y
		}
	}
	return rand.Float64()*w*2 - w, rand.Float64()*h*2 - h
}

// Tick runs one game tick. Returns the diff data for broadcasting.
// tickNum is the current tick number for version tracking.
func (e *Engine) Tick() (updated []*Cell, eaten []EatEvent, removed []uint32, tickNum uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.tickNum++
	e.updated = e.updated[:0]
	e.eaten = e.eaten[:0]

	// Process queued player removals (from WS disconnect handlers).
	// Done here under the Tick lock so grid modifications don't race with Broadcast.
	for _, id := range e.pendingRemovals {
		e.removePlayerLocked(id)
	}
	e.pendingRemovals = e.pendingRemovals[:0]

	// Clear Born flag for cells spawned between ticks (e.g. SpawnPlayer from WS handler).
	// These cells were added to bornThisTick via addCell but never processed by step 9
	// of the previous tick. Without this, Born stays true forever and moveMovableCells
	// never adds them to e.updated (the "frozen cell" bug).
	for _, c := range e.bornThisTick {
		c.Born = false
	}
	e.bornThisTick = e.bornThisTick[:0]

	// e.removed may have cross-tick removals (e.g. from RemovePlayer).
	// Don't clear yet — continue appending this tick's removals to it.

	// 1. Spawn food & viruses to maintain count
	e.spawnFood()
	e.spawnViruses()

	// 2. Process player inputs (split, eject)
	for _, p := range e.players {
		if !p.Alive {
			continue
		}
		e.processPlayerSplit(p)
		e.processPlayerEject(p)
	}

	// 2.5 Build movable list (non-food cells only — skip iterating 2000+ food in move/collision).
	// We still need a single map pass for this, but it's cheaper than the old grid rebuild.
	e.movable = e.movable[:0]
	for _, c := range e.cells {
		if c.Type != CellFood {
			e.movable = append(e.movable, c)
		}
	}

	// 3. Move non-food cells only (food never moves)
	e.moveMovableCells()

	// 3.5 Update grid positions for cells that moved (incremental, not full rebuild).
	// Food is already in the grid from spawn and never moves.
	// Only movable cells (players, ejects, viruses) need grid.Move().
	for _, c := range e.movable {
		e.Grid.Move(c)
	}

	// 4. Resolve collisions & eating (includes food eating by players)
	e.resolveCollisions()

	// 5. Apply decay to large cells
	e.applyDecay()

	// 6. Merge player cells that overlap and are past merge timer
	e.mergeCells()

	// 7. Update player scores
	for _, p := range e.players {
		if p.Alive && len(p.Cells) == 0 {
			p.Alive = false
		}
		if p.Alive {
			p.Score = p.TotalMass()
			if p.Score > p.SessionPeakMass {
				p.SessionPeakMass = p.Score
			}
		}
		if p.SpawnProt > 0 {
			p.SpawnProt--
		}
		// Tick down powerup timers
		if p.SpeedBoostTicks > 0 {
			p.SpeedBoostTicks--
		}
		if p.GhostModeTicks > 0 {
			p.GhostModeTicks--
		}
		if p.FreezeTicks > 0 {
			p.FreezeTicks--
		}
		if p.RecombineTicks > 0 {
			p.RecombineTicks--
			// Actively pull all cells toward mass center for fast recombine
			if p.Alive && len(p.Cells) > 1 {
				e.applyRecombinePull(p)
			}
		}
		if p.MassMagnetTicks > 0 {
			p.MassMagnetTicks--
			// Pull nearby food and eject cells toward player
			if p.Alive && len(p.Cells) > 0 {
				e.applyMassMagnet(p)
			}
		}
	}

	// 8. Update leaderboard periodically
	if time.Since(e.lastLBUpdate) >= e.Cfg.LeaderboardRate {
		e.updateLeaderboard()
		e.lastLBUpdate = time.Now()
	}

	// 9. Clean born flags (only check cells born this tick, not all 2000+)
	for _, c := range e.bornThisTick {
		if c.Born {
			e.updated = append(e.updated, c)
			c.Born = false
		}
	}

	// Snapshot removed and clear for next tick
	removed = append(removed, e.removed...)
	e.removed = e.removed[:0]

	return e.updated, e.eaten, removed, e.tickNum
}

// AllCells returns all cells in the game (for full-sync to new clients).
func (e *Engine) AllCells() []*Cell {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]*Cell, 0, len(e.cells))
	for _, c := range e.cells {
		result = append(result, c)
	}
	return result
}

// RLock acquires a read lock on the engine (for external reads like BR).
func (e *Engine) RLock() { e.mu.RLock() }

// RUnlock releases the read lock.
func (e *Engine) RUnlock() { e.mu.RUnlock() }

// Players returns the player map. Must be called under RLock.
func (e *Engine) Players() map[uint32]*Player {
	return e.players
}

func (e *Engine) spawnFood() {
	toSpawn := e.Cfg.FoodCount - e.foodCount
	if toSpawn <= 0 {
		return
	}
	if toSpawn > e.Cfg.FoodSpawnPer {
		toSpawn = e.Cfg.FoodSpawnPer
	}
	// Spawn food at random positions. Skip expensive sparse-area search
	// to save ~50 grid queries per tick.
	for i := 0; i < toSpawn; i++ {
		c := NewFood(e.Cfg)
		e.addCell(c)
		e.foodCount++
	}
}

func (e *Engine) spawnViruses() {
	for e.virusCount < e.Cfg.VirusCount {
		c := NewVirus(e.Cfg)
		e.addCell(c)
		e.virusCount++
	}
}

// SpawnVirusAt creates a virus at a specific position (used by powerups).
func (e *Engine) SpawnVirusAt(x, y float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := NewVirus(e.Cfg)
	v.X = x
	v.Y = y
	e.addCell(v)
	e.virusCount++
}

// SpawnFreezeSplitter creates a fast-moving eject cell toward mouse that force-splits the first player it hits.
// It uses the virus cell type so it triggers virusPop on collision with players.
func (e *Engine) SpawnFreezeSplitter(owner *Player, fromX, fromY, toX, toY float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dx := toX - fromX
	dy := toY - fromY
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist < 1 {
		dx, dy = 1, 0
		dist = 1
	}
	nx := dx / dist
	ny := dy / dist

	v := NewVirus(e.Cfg)
	v.X = fromX + nx*owner.Cells[0].Size*1.5 // spawn just outside the player
	v.Y = fromY + ny*owner.Cells[0].Size*1.5
	v.VX = nx * 1200.0 * BoostDecayRate // very fast projectile
	v.VY = ny * 1200.0 * BoostDecayRate
	v.Feeder = owner            // attribute to the caster
	v.IsFreezeSplitter = true   // mark as freeze splitter
	e.addCell(v)
	e.virusCount++
}

func (e *Engine) processPlayerSplit(p *Player) {
	if !p.ConsumeSplit() {
		return
	}
	if len(p.Cells) >= e.Cfg.MaxCells {
		return
	}
	// Track daily goal stats
	p.SessionSplits++
	p.SessionNoSplit = false

	// Split each cell that's large enough (snapshot current cells).
	// Sort largest first so that when near max cells, the biggest cells
	// get priority to split (matching expected gameplay behavior).
	snapshot := make([]*Cell, len(p.Cells))
	copy(snapshot, p.Cells)
	sort.Slice(snapshot, func(i, j int) bool {
		return snapshot[i].Size > snapshot[j].Size
	})

	for _, c := range snapshot {
		if len(p.Cells) >= e.Cfg.MaxCells {
			break
		}
		if c.Size < e.Cfg.MinSplitSize {
			continue
		}

		// Halve the parent — parent stays in place, child ejects forward
		newSize := c.Size / math.Sqrt2
		c.Size = newSize
		e.updated = append(e.updated, c) // tell client parent shrunk

		// Direction toward mouse
		dx := p.MouseX - c.X
		dy := p.MouseY - c.Y
		dist := math.Sqrt(dx*dx + dy*dy)
		if dist < 1 {
			dx, dy = 1, 0
			dist = 1
		}
		nx := dx / dist
		ny := dy / dist

		// Spawn child offset forward (like eject) so parent doesn't get pushed back
		child := NewPlayerCell(p, c.X+nx*newSize, c.Y+ny*newSize, newSize)

		// Ogar-style: nearly constant split speed regardless of cell size.
		// splitSpeed ≈ playerSpeed * 2.6 * pow(size, 0.0122) ≈ 80 constant.
		// Total distance = v0 / (1 - decay) ≈ 80 / 0.11 ≈ 727 units.
		child.VX = nx * SplitBoostV0
		child.VY = ny * SplitBoostV0
		child.MergeAt = int64(e.tickNum) + int64(e.Cfg.MergeDelay/e.Cfg.TickRate)

		// Suppress push-apart for 15 ticks (600ms) after split, matching Ogar's collisionRestoreTicks.
		child.NoPushTicks = 15
		c.NoPushTicks = 15

		e.addCell(child)
		p.Cells = append(p.Cells, child)
	}
}

func (e *Engine) processPlayerEject(p *Player) {
	if !p.ConsumeEject() {
		return
	}

	// Server-side rate limit: max 25 ejects/sec at 25Hz tick rate = every tick
	minTickGap := uint64(1) // allow eject every tick
	if e.tickNum-p.lastEjectTick < minTickGap {
		return
	}
	p.lastEjectTick = e.tickNum

	// Derive eject mass from configured eject size (mass = size²/100).
	// Mass-conserving: parent loses exactly what the blob carries.
	ejectSize := e.Cfg.EjectSize
	ejectMass := ejectSize * ejectSize / 100.0

	for _, c := range p.Cells {
		// Don't eject if it would reduce cell below start size
		startMass := e.Cfg.StartSize * e.Cfg.StartSize / 100.0
		if c.Mass()-ejectMass < startMass {
			continue
		}

		// Direction toward mouse with Ogar-style spread: ±0.3 radians (±17.2°)
		dx := p.MouseX - c.X
		dy := p.MouseY - c.Y
		dist := math.Sqrt(dx*dx + dy*dy)
		if dist < 1 {
			dx, dy = 1, 0
			dist = 1
		}
		angle := math.Atan2(dy, dx)
		spread := (rand.Float64()*2 - 1) * EjectSpread
		angle += spread
		nx := math.Cos(angle)
		ny := math.Sin(angle)

		// Mass-conserving: parent loses exactly what the blob carries.
		c.SetMass(c.Mass() - ejectMass)
		e.updated = append(e.updated, c)
		p.SessionMassEject += ejectMass

		// Spawn offset: cell edge + 16 padding (Ogar: cell.getSize() + 16)
		spawnDist := c.Size + EjectSpawnPad
		ej := NewEject(
			c.X+nx*spawnDist, c.Y+ny*spawnDist,
			nx*EjectBoostV0, ny*EjectBoostV0,
			c.R, c.G, c.B,
			ejectSize,
		)
		ej.Owner = p // track who ejected for virus feed attribution
		e.addCell(ej)
	}
}

// Ogar-style boost physics constants.
const (
	BoostDecay     = 0.89             // velocity multiplied by this each tick (matches Ogar)
	BoostDecayRate = 1.0 - BoostDecay // = 0.11
	BoostMinSq     = 2.0              // stop boosting when velocity² < this (Ogar uses distanceSq < 2)
	SplitBoostV0   = 80.0             // initial split speed (Ogar: ~78-82, nearly constant)
	EjectBoostV0   = 100.0            // initial eject speed (Ogar: ejectSpeed = 100)
	EjectMinMass   = 32.0             // minimum cell mass to eject (Ogar: playerMinMassEject = 32)
	EjectSpawnPad  = 16.0             // extra offset from cell edge for spawn position (Ogar: +16)
	EjectSpread    = 0.0524           // random angle spread in radians (±3°)
)

func (e *Engine) moveMovableCells() {
	for _, c := range e.movable {
		moved := false

		// Decrement push-apart suppression timer
		if c.NoPushTicks > 0 {
			c.NoPushTicks--
		}

		// Apply Ogar-style boost: velocity decays exponentially each tick.
		// Player cells ALSO move toward mouse during boost, creating curved paths.
		if c.IsBoosting() {
			c.X += c.VX
			c.Y += c.VY
			c.VX *= BoostDecay
			c.VY *= BoostDecay
			if !c.IsBoosting() {
				c.VX = 0
				c.VY = 0
			}
			moved = true
		}

		// Player cells move toward mouse (even during boost for curved paths)
		if c.Type == CellPlayer && c.Owner != nil {
			p := c.Owner
			p.mu.Lock()
			frozen := p.Frozen
			var mx, my float64
			if p.DirectionLocked {
				// Move along locked direction: project far ahead from cell center
				mx = c.X + p.LockedDirX*10000
				my = c.Y + p.LockedDirY*10000
			} else {
				mx, my = p.MouseX, p.MouseY
			}
			p.mu.Unlock()

			// Frozen players don't move toward mouse at all (X key or freeze splitter)
			if !frozen && p.FreezeTicks <= 0 {
				dx := mx - c.X
				dy := my - c.Y
				dist := math.Sqrt(dx*dx + dy*dy)

				if dist > 1 {
					speed := SpeedForSize(c.Size) * e.Cfg.MoveSpeed * 40 // scale for tick interval
					// Apply speed boost powerup
					if p.SpeedBoostTicks > 0 {
						speed *= 2.0
					}
					if speed > dist {
						speed = dist
					}
					c.X += (dx / dist) * speed
					c.Y += (dy / dist) * speed
					moved = true
				}
			}
		}

		// Border check (Ogar-style: uses half the visual radius as offset,
		// so cells can visually extend past the border by Size/2).
		// Boosting cells (viruses, ejects, split-boosting players) bounce off walls.
		halfW := e.Cfg.MapWidth
		halfH := e.Cfg.MapHeight
		checkR := c.Size / 2 // Ogar: getSize() / 2
		canBounce := c.IsBoosting()

		if c.X-checkR < -halfW {
			c.X = -halfW + checkR
			if canBounce {
				c.VX = -c.VX
			}
			moved = true
		} else if c.X+checkR > halfW {
			c.X = halfW - checkR
			if canBounce {
				c.VX = -c.VX
			}
			moved = true
		}
		if c.Y-checkR < -halfH {
			c.Y = -halfH + checkR
			if canBounce {
				c.VY = -c.VY
			}
			moved = true
		} else if c.Y+checkR > halfH {
			c.Y = halfH - checkR
			if canBounce {
				c.VY = -c.VY
			}
			moved = true
		}

		if moved && !c.Born {
			e.updated = append(e.updated, c)
		}
	}
}

func (e *Engine) resolveCollisions() {
	// Collect only cells that CAN interact (players, viruses, ejects).
	// Use pre-built movable list (non-food cells only).
	// Food is passive — it only gets eaten BY player cells via grid query below.
	active := e.active[:0]
	active = append(active, e.movable...)
	e.active = active

	// Sort active by size descending so larger cells checked first
	sort.Slice(active, func(i, j int) bool {
		return active[i].Size > active[j].Size
	})

	// Reuse toRemove map — clear it
	toRemove := e.toRemove
	for k := range toRemove {
		delete(toRemove, k)
	}

	for _, a := range active {
		if toRemove[a.ID] {
			continue
		}

		// Query grid for nearby cells within interaction range
		maxRange := a.Size * 2.5 // generous search radius

		// Player cells also eat food — use larger radius for that
		if a.Type == CellPlayer {
			if a.Size*1.5 > maxRange {
				maxRange = a.Size * 1.5
			}
		}
		nearby := e.Grid.QueryRadius(a.X, a.Y, maxRange)

		for _, b := range nearby {
			if b.ID == a.ID || toRemove[b.ID] {
				continue
			}

			// Player eating food (fast path — most common interaction)
			if a.Type == CellPlayer && b.Type == CellFood {
				if CanEatFood(a, b) {
					e.eatCell(a, b)
					toRemove[b.ID] = true
					e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
				}
				continue
			}
			// Non-player active cells skip food entirely
			if b.Type == CellFood {
				continue
			}

			// Ensure a is always the larger cell
			if b.Size > a.Size {
				continue // b will process this pair when it's the active cell
			}
			// If same size, use ID to break tie and avoid duplicate
			if b.Size == a.Size && b.ID > a.ID {
				continue
			}

			dx := a.X - b.X
			dy := a.Y - b.Y
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist > a.Size+b.Size {
				continue
			}

			// Same owner player cells — push apart (no eating own cells unless merging)
			if a.Type == CellPlayer && b.Type == CellPlayer && a.Owner != nil && b.Owner != nil && a.Owner == b.Owner {
				if a.MergeAt > int64(e.tickNum) || b.MergeAt > int64(e.tickNum) {
					// Ogar-style: suppress push-apart for a fixed number of ticks
					// after split (collisionRestoreTicks), then push normally.
					if a.NoPushTicks <= 0 && b.NoPushTicks <= 0 {
						e.pushApart(a, b, dist)
					}
					continue
				}
			}

			// Spawn protection
			if b.Type == CellPlayer && b.Owner != nil && b.Owner.SpawnProt > 0 && a.Owner != b.Owner {
				continue
			}
			if a.Type == CellPlayer && a.Owner != nil && a.Owner.SpawnProt > 0 && b.Type == CellPlayer && a.Owner != b.Owner {
				continue
			}

			// Virus + player (Ogar-style: same eating check as any other cell)
			if a.Type == CellPlayer && b.Type == CellVirus {
				if CanEat(a, b) {
					// Credit the player who fed the virus
					if b.Feeder != nil && b.Feeder != a.Owner {
						b.Feeder.SessionVirusHits++
					}
					e.virusPop(a, b)
					toRemove[b.ID] = true
					e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
				}
				continue
			}
			if a.Type == CellVirus && b.Type == CellPlayer {
				continue
			}
			if a.Type == CellVirus && b.Type == CellVirus {
				continue
			}
			if a.Type == CellVirus && b.Type == CellEject {
				if dist < a.Size {
					a.FeedCount++
					if b.Owner != nil {
						a.Feeder = b.Owner // track who fed the virus
					}
					toRemove[b.ID] = true
					e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
					feedsToSplit := int(math.Round((e.Cfg.VirusMaxSize - e.Cfg.VirusSize) / e.Cfg.VirusFeedSize))
					if a.FeedCount >= feedsToSplit {
						e.virusSplit(a, b.VX, b.VY)
					}
				}
				continue
			}
			if a.Type == CellEject && b.Type == CellVirus {
				if dist < b.Size {
					b.FeedCount++
					if a.Owner != nil {
						b.Feeder = a.Owner // track who fed the virus
					}
					toRemove[a.ID] = true
					e.eaten = append(e.eaten, EatEvent{b.ID, a.ID})
					feedsToSplit := int(math.Round((e.Cfg.VirusMaxSize - e.Cfg.VirusSize) / e.Cfg.VirusFeedSize))
					if b.FeedCount >= feedsToSplit {
						e.virusSplit(b, a.VX, a.VY)
					}
				}
				continue
			}

			// Food/eject cannot eat players
			if (a.Type == CellFood || a.Type == CellEject) && b.Type == CellPlayer {
				continue
			}
			if a.Type == CellPlayer && (b.Type == CellFood || b.Type == CellEject) {
				// Player eats food/eject — fall through to eat check
			} else if a.Type == CellFood || a.Type == CellEject {
				continue
			}

			if CanEat(a, b) {
				// Ghost Mode: skip player-vs-player eating when either has ghost mode active
				if a.Type == CellPlayer && b.Type == CellPlayer && a.Owner != b.Owner {
					aGhost := a.Owner != nil && a.Owner.GhostModeTicks > 0
					bGhost := b.Owner != nil && b.Owner.GhostModeTicks > 0
					if aGhost || bGhost {
						continue
					}
				}
				// Track player-kills-player for daily goals
				if a.Type == CellPlayer && b.Type == CellPlayer && a.Owner != nil && b.Owner != nil && a.Owner != b.Owner {
					// Check if victim had only 1 cell left -> this is a kill
					if len(b.Owner.Cells) == 1 {
						a.Owner.SessionKills++
						// Check revenge: did this player kill us recently?
						if tick, ok := a.Owner.RevengeWindow[b.Owner.ID]; ok {
							if e.tickNum-tick < 25*60 { // 60 seconds
								a.Owner.SessionRevenge++
							}
							delete(a.Owner.RevengeWindow, b.Owner.ID)
						}
					}
					// Record in victim's revenge window so they can get revenge later
					b.Owner.RevengeWindow[a.Owner.ID] = e.tickNum
				}
				e.eatCell(a, b)
				toRemove[b.ID] = true
				e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
			}
		}
	}

	// Remove eaten cells
	for id := range toRemove {
		c := e.cells[id]
		if c == nil {
			continue
		}
		if c.Type == CellFood {
			e.foodCount--
		} else if c.Type == CellVirus {
			e.virusCount--
		}
		if c.Owner != nil {
			c.Owner.RemoveCell(id)
		}
		e.Grid.Remove(c)
		delete(e.cells, id)
		e.removed = append(e.removed, id)
	}
}

func (e *Engine) pushApart(a, b *Cell, dist float64) {
	if dist < 1 {
		dist = 1
	}
	overlap := (a.Size + b.Size) - dist
	if overlap <= 0 {
		return
	}
	dx := a.X - b.X
	dy := a.Y - b.Y
	nx := dx / dist
	ny := dy / dist
	// Ogar-style: weighted push — larger cell moves less
	totalSize := a.Size + b.Size
	pushA := overlap * (b.Size / totalSize)
	pushB := overlap * (a.Size / totalSize)
	a.X += nx * pushA
	a.Y += ny * pushA
	b.X -= nx * pushB
	b.Y -= ny * pushB
}

func (e *Engine) eatCell(eater, eaten *Cell) {
	// Transfer mass
	eater.SetMass(eater.Mass() + eaten.Mass())
	e.updated = append(e.updated, eater)
}

func (e *Engine) virusPop(player *Cell, virus *Cell) {
	if player.Owner == nil {
		return
	}
	p := player.Owner

	// Freeze splitter: freeze the target player for 3 seconds (75 ticks)
	if virus.IsFreezeSplitter && virus.Feeder != nil && virus.Feeder != p {
		p.FreezeTicks = 75 // 3 seconds at 25Hz
	}

	// Can't split if already at max cells
	if len(p.Cells) >= e.Cfg.MaxCells {
		e.eatCell(player, virus)
		return
	}

	// OgarII virus pop: absorb virus mass, then repeatedly halve the cell
	// up to VirusSplit times (capped by MaxCells).
	player.Size = math.Sqrt((player.Mass() + virus.Mass()) * 100)
	e.updated = append(e.updated, player)

	splits := e.Cfg.VirusSplit
	available := e.Cfg.MaxCells - len(p.Cells)
	if splits > available {
		splits = available
	}

	mergeAt := int64(e.tickNum) + int64(e.Cfg.MergeDelay/e.Cfg.TickRate)*2

	for i := 0; i < splits; i++ {
		// Halve parent mass each iteration
		curMass := player.Mass()
		halfSize := math.Sqrt(curMass / 2.0 * 100)
		if halfSize < e.Cfg.MinSplitSize {
			break
		}
		player.Size = halfSize

		// Each child flies off in a random direction
		angle := rand.Float64() * 2 * math.Pi
		nx := math.Cos(angle)
		ny := math.Sin(angle)

		child := NewPlayerCell(p, player.X+nx*halfSize, player.Y+ny*halfSize, halfSize)
		child.VX = nx * SplitBoostV0
		child.VY = ny * SplitBoostV0
		child.MergeAt = mergeAt
		child.NoPushTicks = 15

		e.addCell(child)
		p.Cells = append(p.Cells, child)
	}
	player.NoPushTicks = 15
}

func (e *Engine) virusSplit(v *Cell, ejectVX, ejectVY float64) {
	// When a virus gets too large from eating W, it splits in the direction
	// the last eject was traveling.
	angle := math.Atan2(ejectVY, ejectVX)
	child := NewVirus(e.Cfg)
	child.X = v.X
	child.Y = v.Y
	child.Feeder = v.Feeder // propagate feed attribution to child virus
	// Virus split travels ~780 units (OgarII virusSplitBoost = 780)
	const virusSplitV0 = 780.0 * BoostDecayRate
	child.VX = math.Cos(angle) * virusSplitV0
	child.VY = math.Sin(angle) * virusSplitV0
	e.addCell(child)
	e.virusCount++

	v.Size = e.Cfg.VirusSize
	v.FeedCount = 0
	v.Feeder = nil // reset parent virus feeder
}

func (e *Engine) applyDecay() {
	minSize := e.Cfg.StartSize
	for _, p := range e.players {
		if !p.Alive {
			continue
		}
		for _, c := range p.Cells {
			if c.Size > minSize {
				c.Size *= e.Cfg.DecayRate
				if c.Size < minSize {
					c.Size = minSize
				}
				e.updated = append(e.updated, c)
			}
		}
	}
}

// applyMassMagnet pulls nearby food, eject cells, AND enemy mass toward a player with the magnet powerup.
func (e *Engine) applyMassMagnet(p *Player) {
	const magnetRadius = 800.0  // increased pull range
	const pullStrength = 12.0   // stronger pull per tick
	const enemyPullStrength = 4.0 // gentler pull on enemy cells
	cx, cy := p.Center()
	for _, c := range e.cells {
		switch c.Type {
		case CellFood, CellEject:
			// Don't pull own eject cells
			if c.Type == CellEject && c.Owner == p {
				continue
			}
			dx := cx - c.X
			dy := cy - c.Y
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist < magnetRadius && dist > 1 {
				nx := dx / dist
				ny := dy / dist
				c.X += nx * pullStrength
				c.Y += ny * pullStrength
				e.Grid.Move(c)
			}
		case CellPlayer:
			// Pull enemy player cells toward us (not our own)
			if c.Owner == p || c.Owner == nil {
				continue
			}
			// Don't pull ghost-mode players
			if c.Owner.GhostModeTicks > 0 {
				continue
			}
			dx := cx - c.X
			dy := cy - c.Y
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist < magnetRadius && dist > 1 {
				nx := dx / dist
				ny := dy / dist
				c.X += nx * enemyPullStrength
				c.Y += ny * enemyPullStrength
				e.Grid.Move(c)
			}
		}
	}
}

// applyRecombinePull pulls all of a player's cells toward their mass center for fast merging.
func (e *Engine) applyRecombinePull(p *Player) {
	cx, cy := p.Center()
	for _, c := range p.Cells {
		dx := cx - c.X
		dy := cy - c.Y
		dist := math.Sqrt(dx*dx + dy*dy)
		if dist > 1 {
			// Strong pull toward center — 30% of distance per tick
			pull := dist * 0.3
			nx := dx / dist
			ny := dy / dist
			c.X += nx * pull
			c.Y += ny * pull
			e.Grid.Move(c)
		}
		// Keep merge timer cleared during active recombine
		c.MergeAt = 0
	}
}

func (e *Engine) mergeCells() {
	for _, p := range e.players {
		if len(p.Cells) <= 1 {
			continue
		}
		merged := make(map[uint32]bool)
		for i := 0; i < len(p.Cells); i++ {
			a := p.Cells[i]
			if merged[a.ID] {
				continue
			}
			if a.MergeAt > int64(e.tickNum) {
				continue
			}
			for j := i + 1; j < len(p.Cells); j++ {
				b := p.Cells[j]
				if merged[b.ID] {
					continue
				}
				if b.MergeAt > int64(e.tickNum) {
					continue
				}
				dx := a.X - b.X
				dy := a.Y - b.Y
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist < a.Size*0.5 {
					// Merge b into a
					a.SetMass(a.Mass() + b.Mass())
					merged[b.ID] = true
					e.Grid.Remove(b)
					delete(e.cells, b.ID)
					e.removed = append(e.removed, b.ID)
					e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
				}
			}
		}
		// Remove merged cells from player
		if len(merged) > 0 {
			alive := p.Cells[:0]
			for _, c := range p.Cells {
				if !merged[c.ID] {
					alive = append(alive, c)
				}
			}
			p.Cells = alive
		}
	}
}

func (e *Engine) updateLeaderboard() {
	entries := make([]LeaderEntry, 0, len(e.players))
	for _, p := range e.players {
		if !p.Alive {
			continue
		}
		entries = append(entries, LeaderEntry{
			Name:         p.Name,
			Score:        p.TotalMass(),
			IsSubscriber: p.IsSubscriber,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})
	if len(entries) > e.Cfg.LeaderboardSize {
		entries = entries[:e.Cfg.LeaderboardSize]
	}
	e.Leaderboard = entries
}

// GetAllCells returns a snapshot of all cells (for initial world state).
func (e *Engine) GetAllCells() []*Cell {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cells := make([]*Cell, 0, len(e.cells))
	for _, c := range e.cells {
		cells = append(cells, c)
	}
	return cells
}

// CellsInViewport returns all cells visible within the given viewport.
// Uses the spatial grid for fast lookup instead of scanning all cells.
// Must be called while holding at least a read lock.
func (e *Engine) CellsInViewport(v Viewport) []*Cell {
	// Use grid query with a margin for cell radius (largest cells ~500)
	candidates := e.Grid.QueryRect(nil, v.Left, v.Top, v.Right, v.Bottom, 500)
	result := make([]*Cell, 0, len(candidates))
	for _, c := range candidates {
		if CellInViewport(c, v) {
			result = append(result, c)
		}
	}
	return result
}

// GetCellsInViewport returns all cells visible within the given viewport (acquires read lock).
func (e *Engine) GetCellsInViewport(v Viewport) []*Cell {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.CellsInViewport(v)
}

// CellExists checks whether a cell with the given ID exists.
func (e *Engine) CellExists(id uint32) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.cells[id]
	return ok
}

// GetPlayer returns a player by ID.
func (e *Engine) GetPlayer(id uint32) *Player {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.players[id]
}

// GetPlayers returns all players.
func (e *Engine) GetPlayers() []*Player {
	e.mu.RLock()
	defer e.mu.RUnlock()
	pls := make([]*Player, 0, len(e.players))
	for _, p := range e.players {
		pls = append(pls, p)
	}
	return pls
}

// PlayerCount returns the number of connected players.
func (e *Engine) PlayerCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.players)
}
