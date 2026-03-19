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
	grid    *SpatialGrid // spatial hash for fast queries

	// Running counters (avoid O(N) scans)
	foodCount  int
	virusCount int

	// Pre-allocated collision buffers (reused each tick)
	active   []*Cell
	toRemove map[uint32]bool

	// Per-tick diff for protocol broadcast
	tickNum uint64
	updated []*Cell    // cells that were added or moved this tick
	eaten   []EatEvent // eat events this tick
	removed []uint32   // cell IDs removed this tick

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
		players:  make(map[uint32]*Player, 64),
		grid:     NewSpatialGrid(cfg.MapWidth, cfg.MapHeight, 500),
		active:   make([]*Cell, 0, 256),
		toRemove: make(map[uint32]bool, 64),
	}
	e.spawnInitialFood()
	e.spawnInitialViruses()
	return e
}

func (e *Engine) spawnInitialFood() {
	for i := 0; i < e.Cfg.FoodCount; i++ {
		c := NewFood(e.Cfg)
		e.cells[c.ID] = c
	}
	e.foodCount = e.Cfg.FoodCount
}

func (e *Engine) spawnInitialViruses() {
	for i := 0; i < e.Cfg.VirusCount; i++ {
		c := NewVirus(e.Cfg)
		e.cells[c.ID] = c
	}
	e.virusCount = e.Cfg.VirusCount
}

// AddPlayer adds a player to the game (does not spawn cells yet).
func (e *Engine) AddPlayer(p *Player) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.players[p.ID] = p
}

// RemovePlayer removes a player and all their cells.
func (e *Engine) RemovePlayer(id uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[id]
	if !ok {
		return
	}
	// Remove all cells
	for _, c := range p.Cells {
		e.removed = append(e.removed, c.ID)
		delete(e.cells, c.ID)
	}
	p.Cells = nil
	p.Alive = false
	delete(e.players, id)
}

// SpawnPlayer spawns a player into the world with one cell.
func (e *Engine) SpawnPlayer(p *Player) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Find a safe spawn position (away from large cells)
	x, y := e.findSpawnPos()

	cell := NewPlayerCell(p, x, y, e.Cfg.StartSize)
	cell.MergeAt = 0
	e.cells[cell.ID] = cell

	p.Cells = append(p.Cells[:0], cell)
	p.Alive = true
	p.Score = cell.Mass()
	p.SpawnProt = 75 // ~3 seconds of spawn protection at 25Hz
}

func (e *Engine) findSpawnPos() (float64, float64) {
	w := e.Cfg.MapWidth * 0.8
	h := e.Cfg.MapHeight * 0.8
	for attempt := 0; attempt < 20; attempt++ {
		x := rand.Float64()*w*2 - w
		y := rand.Float64()*h*2 - h
		safe := true
		for _, c := range e.cells {
			if c.Type == CellPlayer && c.Size > 100 {
				dx := c.X - x
				dy := c.Y - y
				if math.Sqrt(dx*dx+dy*dy) < c.Size*3 {
					safe = false
					break
				}
			}
		}
		if safe {
			return x, y
		}
	}
	return rand.Float64()*w*2 - w, rand.Float64()*h*2 - h
}

// Tick runs one game tick. Returns the diff data for broadcasting.
func (e *Engine) Tick() (updated []*Cell, eaten []EatEvent, removed []uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.tickNum++
	e.updated = e.updated[:0]
	e.eaten = e.eaten[:0]
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

	// 3. Move all cells
	e.moveAllCells()

	// 3.5 Rebuild spatial grid (after movement, before collisions & viewport)
	e.rebuildGrid()

	// 4. Resolve collisions & eating
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
		}
		if p.SpawnProt > 0 {
			p.SpawnProt--
		}
	}

	// 8. Update leaderboard periodically
	if time.Since(e.lastLBUpdate) >= e.Cfg.LeaderboardRate {
		e.updateLeaderboard()
		e.lastLBUpdate = time.Now()
	}

	// 9. Clean born flags
	for _, c := range e.cells {
		if c.Born {
			e.updated = append(e.updated, c)
			c.Born = false
		}
	}

	// Snapshot removed and clear for next tick
	removed = append(removed, e.removed...)
	e.removed = e.removed[:0]

	return e.updated, e.eaten, removed
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

func (e *Engine) spawnFood() {
	toSpawn := e.Cfg.FoodCount - e.foodCount
	if toSpawn <= 0 {
		return
	}
	if toSpawn > e.Cfg.FoodSpawnPer {
		toSpawn = e.Cfg.FoodSpawnPer
	}
	// Use the grid (still populated from last tick) to find sparse areas.
	// For each food to spawn, pick the least-dense spot among a few random candidates.
	checkRadius := e.Cfg.MapWidth * 2 / 20 // ~5% of map width
	for i := 0; i < toSpawn; i++ {
		c := NewFood(e.Cfg)
		bestX, bestY := c.X, c.Y
		bestCount := len(e.grid.QueryRadius(c.X, c.Y, checkRadius))
		// Try a few more random positions and pick the sparsest
		for attempt := 0; attempt < 4; attempt++ {
			rx := rand.Float64()*e.Cfg.MapWidth*2 - e.Cfg.MapWidth
			ry := rand.Float64()*e.Cfg.MapHeight*2 - e.Cfg.MapHeight
			cnt := len(e.grid.QueryRadius(rx, ry, checkRadius))
			if cnt < bestCount {
				bestX, bestY = rx, ry
				bestCount = cnt
			}
		}
		c.X = bestX
		c.Y = bestY
		e.cells[c.ID] = c
		e.grid.Insert(c) // add to grid so subsequent spawns see it
		e.foodCount++
	}
}

func (e *Engine) spawnViruses() {
	for e.virusCount < e.Cfg.VirusCount {
		c := NewVirus(e.Cfg)
		e.cells[c.ID] = c
		e.virusCount++
	}
}

func (e *Engine) processPlayerSplit(p *Player) {
	if !p.ConsumeSplit() {
		return
	}
	if len(p.Cells) >= e.Cfg.MaxCells {
		return
	}

	// Split each cell that's large enough (snapshot current cells)
	snapshot := make([]*Cell, len(p.Cells))
	copy(snapshot, p.Cells)

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

		e.cells[child.ID] = child
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

		// Spawn offset: cell edge + 16 padding (Ogar: cell.getSize() + 16)
		spawnDist := c.Size + EjectSpawnPad
		ej := NewEject(
			c.X+nx*spawnDist, c.Y+ny*spawnDist,
			nx*EjectBoostV0, ny*EjectBoostV0,
			c.R, c.G, c.B,
			ejectSize,
		)
		e.cells[ej.ID] = ej
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

func (e *Engine) moveAllCells() {
	for _, c := range e.cells {
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
			mx, my := p.MouseX, p.MouseY
			p.mu.Unlock()

			dx := mx - c.X
			dy := my - c.Y
			dist := math.Sqrt(dx*dx + dy*dy)

			if dist > 1 {
				speed := SpeedForSize(c.Size) * e.Cfg.MoveSpeed * 40 // scale for tick interval
				if speed > dist {
					speed = dist
				}
				c.X += (dx / dist) * speed
				c.Y += (dy / dist) * speed
				moved = true
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

func (e *Engine) rebuildGrid() {
	e.grid.Clear()
	for _, c := range e.cells {
		e.grid.Insert(c)
	}
}

func (e *Engine) resolveCollisions() {
	// Collect only cells that CAN interact (players, viruses, ejects).
	// Food is passive — it only gets eaten BY players, so we check food
	// from the player's perspective using the grid.
	active := e.active[:0]
	for _, c := range e.cells {
		if c.Type != CellFood {
			active = append(active, c)
		}
	}
	e.active = active

	// Also include all player cells (they eat food via grid query)
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
		nearby := e.grid.QueryRadius(a.X, a.Y, maxRange)

		for _, b := range nearby {
			if b.ID == a.ID || toRemove[b.ID] {
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

			// Same owner — push apart (no eating own cells unless merging)
			if a.Owner != nil && b.Owner != nil && a.Owner == b.Owner {
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
			// Virus ignores regular food entirely
			if a.Type == CellVirus && b.Type == CellFood {
				continue
			}
			if a.Type == CellFood && b.Type == CellVirus {
				continue
			}
			if a.Type == CellVirus && b.Type == CellEject {
				if dist < a.Size {
					a.FeedCount++
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
				e.eatCell(a, b)
				toRemove[b.ID] = true
				e.eaten = append(e.eaten, EatEvent{a.ID, b.ID})
			}
		}
	}

	// Also: player cells eat food via grid (food was excluded from active list)
	for _, p := range e.players {
		if !p.Alive {
			continue
		}
		for _, pc := range p.Cells {
			if toRemove[pc.ID] {
				continue
			}
			nearby := e.grid.QueryRadius(pc.X, pc.Y, pc.Size*1.5)
			for _, food := range nearby {
				if food.Type != CellFood || toRemove[food.ID] {
					continue
				}
				if CanEatFood(pc, food) {
					e.eatCell(pc, food)
					toRemove[food.ID] = true
					e.eaten = append(e.eaten, EatEvent{pc.ID, food.ID})
				}
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

	// Can't split if already at max cells
	if len(p.Cells) >= e.Cfg.MaxCells {
		e.eatCell(player, virus)
		return
	}

	// Virus pop: split the hit cell into exactly 2 pieces
	// Absorb virus mass, then split in half
	totalMass := player.Mass() + virus.Mass()
	newSize := math.Sqrt(totalMass / 2.0 * 100)

	player.Size = newSize
	e.updated = append(e.updated, player)

	// Child ejects in a random direction (virus explosion)
	angle := rand.Float64() * 2 * math.Pi
	nx := math.Cos(angle)
	ny := math.Sin(angle)

	child := NewPlayerCell(p, player.X+nx*newSize, player.Y+ny*newSize, newSize)
	// Virus pop boost: same split speed as normal split (Ogar-style constant)
	child.VX = nx * SplitBoostV0
	child.VY = ny * SplitBoostV0
	child.MergeAt = int64(e.tickNum) + int64(e.Cfg.MergeDelay/e.Cfg.TickRate)*2
	child.NoPushTicks = 15
	player.NoPushTicks = 15

	e.cells[child.ID] = child
	p.Cells = append(p.Cells, child)
}

func (e *Engine) virusSplit(v *Cell, ejectVX, ejectVY float64) {
	// When a virus gets too large from eating W, it splits in the direction
	// the last eject was traveling.
	angle := math.Atan2(ejectVY, ejectVX)
	child := NewVirus(e.Cfg)
	child.X = v.X
	child.Y = v.Y
	// Virus split travels a fixed distance
	const virusSplitV0 = 600.0 * BoostDecayRate
	child.VX = math.Cos(angle) * virusSplitV0
	child.VY = math.Sin(angle) * virusSplitV0
	e.cells[child.ID] = child
	e.virusCount++

	v.Size = e.Cfg.VirusSize
	v.FeedCount = 0
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
	candidates := e.grid.QueryRect(v.Left, v.Top, v.Right, v.Bottom, 500)
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
