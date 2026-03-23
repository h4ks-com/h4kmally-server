package game

import (
	"math"
	"sync"
	"time"
)

// ── Battle Royale State Machine ─────────────────────────────
//
// States: Inactive → Countdown → Active → Finished
//
// Admin triggers via API → starts Countdown (10s).
// Active: zone shrinks from full map to a point over the configured duration.
// Players outside the zone lose mass every tick.
// When only 1 player (or 0) remains alive, the round finishes.

// BRState represents the battle royale game phase.
type BRState int

const (
	BRInactive  BRState = iota // No BR active
	BRCountdown                // Countdown before start
	BRActive                   // Zone is shrinking, damage is active
	BRFinished                 // Round ended, showing results
)

// BattleRoyale manages the BR event state.
type BattleRoyale struct {
	mu sync.RWMutex

	State     BRState
	StartTime time.Time // When active phase began
	EndTime   time.Time // When round ended

	// Countdown
	CountdownStart time.Time
	CountdownSecs  int // seconds of countdown (default 10)

	// Zone configuration
	ZoneDuration  time.Duration // how long the zone takes to fully shrink (default 5 min)
	InitialX      float64       // center of initial zone (map center)
	InitialY      float64
	InitialRadius float64 // starting radius (half map diagonal)
	FinalRadius   float64 // ending radius (tiny circle)
	FinalX        float64 // final center (can drift toward random point)
	FinalY        float64

	// Damage
	DamagePerTick float64 // mass lost per tick when outside zone

	// Tracking
	PlayersAlive int
	WinnerName   string
	WinnerID     uint32

	// Callback for broadcasting messages
	BroadcastFn func(msg string)
}

// NewBattleRoyale creates a new inactive BR instance.
func NewBattleRoyale() *BattleRoyale {
	return &BattleRoyale{
		State:         BRInactive,
		CountdownSecs: 10,
		ZoneDuration:  5 * time.Minute,
		DamagePerTick: 2.0, // mass units per tick (25Hz = 50 mass/sec)
		FinalRadius:   200,
	}
}

// Start begins the BR countdown. Called by admin.
func (br *BattleRoyale) Start(mapHalfW float64) {
	br.mu.Lock()
	defer br.mu.Unlock()

	if br.State != BRInactive && br.State != BRFinished {
		return // already running
	}

	br.State = BRCountdown
	br.CountdownStart = time.Now()
	br.WinnerName = ""
	br.WinnerID = 0
	br.EndTime = time.Time{}

	// Zone starts as full map (circle inscribing the square map)
	br.InitialX = 0
	br.InitialY = 0
	br.InitialRadius = mapHalfW * math.Sqrt2
	br.FinalX = (rand_float64()*2.0 - 1.0) * mapHalfW * 0.3 // random final center
	br.FinalY = (rand_float64()*2.0 - 1.0) * mapHalfW * 0.3

	if br.BroadcastFn != nil {
		br.BroadcastFn("[BR] Battle Royale starting in 10 seconds!")
	}
}

// Stop cancels/ends the BR.
func (br *BattleRoyale) Stop() {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.State = BRInactive
	br.WinnerName = ""
}

// Tick processes one BR game tick. Called from the engine's tick loop.
// Returns: outsidePlayers (player IDs outside zone to apply damage to)
func (br *BattleRoyale) Tick(players map[uint32]*Player) {
	br.mu.Lock()
	defer br.mu.Unlock()

	switch br.State {
	case BRCountdown:
		elapsed := time.Since(br.CountdownStart)
		remaining := br.CountdownSecs - int(elapsed.Seconds())
		if remaining <= 0 {
			// Start active phase
			br.State = BRActive
			br.StartTime = time.Now()
			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Battle Royale has begun! Stay inside the zone!")
			}
		} else if remaining <= 5 && int(elapsed.Seconds()) != int((elapsed-40*time.Millisecond).Seconds()) {
			// Announce countdown every second for last 5 seconds
			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Starting in " + itoa(remaining) + "...")
			}
		}

	case BRActive:
		// Count alive players
		alive := 0
		var lastAlivePlayer *Player
		for _, p := range players {
			if p.Alive && len(p.Cells) > 0 {
				alive++
				lastAlivePlayer = p
			}
		}
		br.PlayersAlive = alive

		// Check win condition
		if alive <= 1 {
			br.State = BRFinished
			br.EndTime = time.Now()
			if alive == 1 && lastAlivePlayer != nil {
				br.WinnerName = lastAlivePlayer.Name
				br.WinnerID = lastAlivePlayer.ID
				if br.BroadcastFn != nil {
					br.BroadcastFn("[BR] " + lastAlivePlayer.Name + " wins the Battle Royale!")
				}
			} else {
				if br.BroadcastFn != nil {
					br.BroadcastFn("[BR] Battle Royale ended — no survivors!")
				}
			}
			return
		}

		// Apply zone damage
		cx, cy, radius := br.currentZone()
		for _, p := range players {
			if !p.Alive || len(p.Cells) == 0 {
				continue
			}
			// Check if ANY of the player's cells are outside the zone
			for _, c := range p.Cells {
				dx := c.X - cx
				dy := c.Y - cy
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > radius {
					// Apply damage: shrink the cell
					c.Size -= br.DamagePerTick
					if c.Size < 10 {
						c.Size = 10 // minimum size before engine removes
					}
				}
			}
		}

	case BRFinished:
		// Auto-reset after 15 seconds
		if time.Since(br.EndTime) > 15*time.Second {
			br.State = BRInactive
			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Battle Royale has ended.")
			}
		}
	}
}

// GetZone returns the current zone center and radius. Thread-safe.
func (br *BattleRoyale) GetZone() (cx, cy, radius float64, state BRState) {
	br.mu.RLock()
	defer br.mu.RUnlock()

	if br.State == BRActive {
		cx, cy, radius = br.currentZone()
	} else if br.State == BRCountdown {
		cx = br.InitialX
		cy = br.InitialY
		radius = br.InitialRadius
	}
	state = br.State
	return
}

// GetInfo returns BR status info for client display.
func (br *BattleRoyale) GetInfo() BRInfo {
	br.mu.RLock()
	defer br.mu.RUnlock()

	info := BRInfo{
		State:        int(br.State),
		PlayersAlive: br.PlayersAlive,
		WinnerName:   br.WinnerName,
	}

	switch br.State {
	case BRCountdown:
		remaining := br.CountdownSecs - int(time.Since(br.CountdownStart).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		info.Countdown = remaining
	case BRActive:
		cx, cy, radius := br.currentZone()
		info.ZoneCX = cx
		info.ZoneCY = cy
		info.ZoneRadius = radius
		elapsed := time.Since(br.StartTime)
		remaining := br.ZoneDuration - elapsed
		if remaining < 0 {
			remaining = 0
		}
		info.TimeRemaining = int(remaining.Seconds())
	}

	return info
}

// BRInfo is the JSON-serializable BR status sent to clients.
type BRInfo struct {
	State         int     `json:"state"` // 0=inactive, 1=countdown, 2=active, 3=finished
	Countdown     int     `json:"countdown,omitempty"`
	PlayersAlive  int     `json:"playersAlive"`
	ZoneCX        float64 `json:"zoneCX,omitempty"`
	ZoneCY        float64 `json:"zoneCY,omitempty"`
	ZoneRadius    float64 `json:"zoneRadius,omitempty"`
	TimeRemaining int     `json:"timeRemaining,omitempty"`
	WinnerName    string  `json:"winnerName,omitempty"`
}

// currentZone calculates current zone position and radius (must be called under lock).
func (br *BattleRoyale) currentZone() (cx, cy, radius float64) {
	elapsed := time.Since(br.StartTime)
	progress := elapsed.Seconds() / br.ZoneDuration.Seconds()
	if progress > 1 {
		progress = 1
	}

	// Smooth easing (ease-in-out)
	t := progress

	// Linearly interpolate center
	cx = br.InitialX + (br.FinalX-br.InitialX)*t
	cy = br.InitialY + (br.FinalY-br.InitialY)*t

	// Linearly interpolate radius
	radius = br.InitialRadius + (br.FinalRadius-br.InitialRadius)*t
	return
}

// Simple int-to-string without importing strconv
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	result := ""
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	for n > 0 {
		result = string(rune('0'+n%10)) + result
		n /= 10
	}
	if negative {
		result = "-" + result
	}
	return result
}

// Deterministic-enough random float for zone center offset
var brRandMu sync.Mutex
var brRandState uint64 = uint64(time.Now().UnixNano())

func rand_float64() float64 {
	brRandMu.Lock()
	defer brRandMu.Unlock()
	// xorshift64
	brRandState ^= brRandState << 13
	brRandState ^= brRandState >> 7
	brRandState ^= brRandState << 17
	return float64(brRandState%10000) / 10000.0
}
