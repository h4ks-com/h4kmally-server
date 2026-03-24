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
// Active: zone shrinks from full map to a point over the configured duration,
// then continues collapsing to zero in a 30-second sudden death.
// Player cells outside the zone take damage each tick;
// when all cells of a player reach kill threshold, the player is eliminated.
// Last player standing wins.

// BRState represents the battle royale game phase.
type BRState int

const (
	BRInactive  BRState = iota // No BR active
	BRCountdown                // Countdown before start
	BRActive                   // Zone is shrinking, damage is active
	BRFinished                 // Round ended, showing results
)

// BRKillThreshold is the minimum cell size before a cell is considered dead from zone damage.
const BRKillThreshold = 20.0

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
	ZoneDuration  time.Duration // how long the zone takes to reach FinalRadius (default 5 min)
	InitialX      float64       // center of initial zone (map center)
	InitialY      float64
	InitialRadius float64 // starting radius (half map diagonal)
	FinalRadius   float64 // ending radius at end of ZoneDuration
	FinalX        float64 // final center (can drift toward random point)
	FinalY        float64

	// Sudden death: after ZoneDuration, zone keeps shrinking to 0 over this period
	SuddenDeathDuration time.Duration

	// Damage
	DamagePerTick float64 // size units lost per tick when outside zone

	// Tracking
	PlayersAlive int
	WinnerName   string
	WinnerID     uint32

	// Placement tracking: maps player ID → placement (1st, 2nd, 3rd...)
	Placements map[uint32]int
	nextPlace  int // next placement to assign (counts down: N, N-1, ... 1)

	// Callback for broadcasting messages
	BroadcastFn func(msg string)

	// Whether placement rewards have been granted for the current round
	RewardsGranted bool

	// Auto-scheduling
	AutoEnabled  bool          // whether automatic BR rounds are enabled
	AutoInterval time.Duration // interval between auto-starts (default 1 hour)
	LastAutoEnd  time.Time     // when the last BR round ended (for scheduling next)
}

// NewBattleRoyale creates a new inactive BR instance.
func NewBattleRoyale() *BattleRoyale {
	return &BattleRoyale{
		State:               BRInactive,
		CountdownSecs:       10,
		ZoneDuration:        5 * time.Minute,
		SuddenDeathDuration: 30 * time.Second,
		DamagePerTick:       1.5, // size units per tick (25Hz → ~37.5 size/sec)
		FinalRadius:         200,
		Placements:          make(map[uint32]int),
		AutoEnabled:         false,
		AutoInterval:        60 * time.Minute,
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
	br.Placements = make(map[uint32]int)
	br.nextPlace = 0 // will be set when active phase begins
	br.RewardsGranted = false

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

// IsActive returns true if BR is in countdown or active state.
func (br *BattleRoyale) IsActive() bool {
	br.mu.RLock()
	defer br.mu.RUnlock()
	return br.State == BRCountdown || br.State == BRActive
}

// IsActivePhase returns true only during the active (fighting) phase — not countdown.
func (br *BattleRoyale) IsActivePhase() bool {
	br.mu.RLock()
	defer br.mu.RUnlock()
	return br.State == BRActive
}

// EstimatedSecondsRemaining returns an approximate number of seconds until the
// current BR round ends. Returns 0 if not active.
func (br *BattleRoyale) EstimatedSecondsRemaining() int {
	br.mu.RLock()
	defer br.mu.RUnlock()
	switch br.State {
	case BRCountdown:
		countdownLeft := br.CountdownSecs - int(time.Since(br.CountdownStart).Seconds())
		if countdownLeft < 0 {
			countdownLeft = 0
		}
		return countdownLeft + int(br.ZoneDuration.Seconds()) + int(br.SuddenDeathDuration.Seconds())
	case BRActive:
		elapsed := time.Since(br.StartTime)
		total := br.ZoneDuration + br.SuddenDeathDuration
		remaining := total - elapsed
		if remaining < 0 {
			return 0
		}
		return int(remaining.Seconds())
	default:
		return 0
	}
}

// SetAutoConfig updates auto-scheduling settings. Thread-safe.
func (br *BattleRoyale) SetAutoConfig(enabled bool, intervalMinutes int) {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.AutoEnabled = enabled
	if intervalMinutes > 0 {
		br.AutoInterval = time.Duration(intervalMinutes) * time.Minute
	}
}

// GetAutoConfig returns the current auto-scheduling config. Thread-safe.
func (br *BattleRoyale) GetAutoConfig() (enabled bool, intervalMinutes int) {
	br.mu.RLock()
	defer br.mu.RUnlock()
	return br.AutoEnabled, int(br.AutoInterval.Minutes())
}

// CheckAutoStart returns true if it's time to auto-start a new BR round.
// Should be called from the tick loop.
func (br *BattleRoyale) CheckAutoStart() bool {
	br.mu.RLock()
	defer br.mu.RUnlock()
	if !br.AutoEnabled {
		return false
	}
	if br.State != BRInactive {
		return false
	}
	if br.LastAutoEnd.IsZero() {
		// First auto round — start immediately
		return true
	}
	return time.Since(br.LastAutoEnd) >= br.AutoInterval
}

// GetPlacement returns the placement for a player (0 if not eliminated yet).
func (br *BattleRoyale) GetPlacement(playerID uint32) int {
	br.mu.RLock()
	defer br.mu.RUnlock()
	return br.Placements[playerID]
}

// Tick processes one BR game tick. Called from the game loop.
// Returns a list of player IDs that should be killed (all cells removed) this tick.
func (br *BattleRoyale) Tick(players map[uint32]*Player) []uint32 {
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

			// Count initial alive players for placement tracking
			alive := 0
			for _, p := range players {
				if p.Alive && len(p.Cells) > 0 {
					alive++
				}
			}
			br.nextPlace = alive
			br.PlayersAlive = alive

			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Battle Royale has begun! Stay inside the zone!")
			}
		} else if remaining <= 5 && int(elapsed.Seconds()) != int((elapsed-40*time.Millisecond).Seconds()) {
			// Announce countdown every second for last 5 seconds
			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Starting in " + itoa(remaining) + "...")
			}
		}
		return nil

	case BRActive:
		// Get current zone
		cx, cy, radius := br.currentZone()

		// Apply zone damage and collect kills
		var kills []uint32
		for _, p := range players {
			if !p.Alive || len(p.Cells) == 0 {
				continue
			}

			allBelowThreshold := true
			for _, c := range p.Cells {
				dx := c.X - cx
				dy := c.Y - cy
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist+c.Size > radius {
					// Cell is at least partially outside zone — apply damage
					// Damage scales with how far outside the zone the cell is
					overshoot := (dist + c.Size) - radius
					dmgScale := 1.0 + overshoot/(radius*0.5+1)
					if dmgScale > 3.0 {
						dmgScale = 3.0
					}
					c.Size -= br.DamagePerTick * dmgScale
				}
				if c.Size > BRKillThreshold {
					allBelowThreshold = false
				}
			}

			// If ALL cells are below kill threshold, this player is eliminated
			if allBelowThreshold {
				kills = append(kills, p.ID)
				if br.nextPlace > 0 {
					br.Placements[p.ID] = br.nextPlace
					br.nextPlace--
				}
				if br.BroadcastFn != nil {
					place := br.Placements[p.ID]
					br.BroadcastFn("[BR] " + p.Name + " eliminated! (#" + itoa(place) + ")")
				}
			}
		}

		// Recount alive players (subtract kills that are about to happen)
		alive := 0
		for _, p := range players {
			if !p.Alive || len(p.Cells) == 0 {
				continue
			}
			// Check if this player is in the kill list
			killed := false
			for _, kid := range kills {
				if kid == p.ID {
					killed = true
					break
				}
			}
			if !killed {
				alive++
			}
		}
		br.PlayersAlive = alive

		// Check win condition
		if alive <= 1 {
			br.State = BRFinished
			br.EndTime = time.Now()
			if alive == 1 {
				// Find the winner
				for _, p := range players {
					if !p.Alive || len(p.Cells) == 0 {
						continue
					}
					killed := false
					for _, kid := range kills {
						if kid == p.ID {
							killed = true
							break
						}
					}
					if !killed {
						br.WinnerName = p.Name
						br.WinnerID = p.ID
						br.Placements[p.ID] = 1
						if br.BroadcastFn != nil {
							br.BroadcastFn("[BR] " + p.Name + " wins the Battle Royale! \xF0\x9F\x8F\x86")
						}
						break
					}
				}
			} else {
				if br.BroadcastFn != nil {
					br.BroadcastFn("[BR] Battle Royale ended — no survivors!")
				}
			}
		}

		return kills

	case BRFinished:
		// Auto-reset after 15 seconds
		if time.Since(br.EndTime) > 15*time.Second {
			br.State = BRInactive
			br.LastAutoEnd = time.Now()
			if br.BroadcastFn != nil {
				br.BroadcastFn("[BR] Battle Royale has ended.")
			}
		}
		return nil
	}

	return nil
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
		if elapsed < br.ZoneDuration {
			remaining := br.ZoneDuration - elapsed
			info.TimeRemaining = int(remaining.Seconds())
		} else {
			// Sudden death phase — show remaining SD time (negative = overtime indicator)
			overtime := elapsed - br.ZoneDuration
			sdRemaining := br.SuddenDeathDuration - overtime
			if sdRemaining < 0 {
				sdRemaining = 0
			}
			info.TimeRemaining = -int(sdRemaining.Seconds()) - 1 // negative = sudden death
		}
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

// GetTopPlacements returns player IDs for 1st, 2nd, and 3rd place.
// Returns 0 for any placement not assigned. Thread-safe.
func (br *BattleRoyale) GetTopPlacements() (first, second, third uint32) {
	br.mu.RLock()
	defer br.mu.RUnlock()
	for pid, place := range br.Placements {
		switch place {
		case 1:
			first = pid
		case 2:
			second = pid
		case 3:
			third = pid
		}
	}
	return
}

// MarkRewardsGranted sets the flag to prevent double-granting rewards.
func (br *BattleRoyale) MarkRewardsGranted() {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.RewardsGranted = true
}

// AreRewardsGranted returns whether rewards have already been granted.
func (br *BattleRoyale) AreRewardsGranted() bool {
	br.mu.RLock()
	defer br.mu.RUnlock()
	return br.RewardsGranted
}

// currentZone calculates current zone position and radius (must be called under lock).
// After ZoneDuration, the zone enters sudden death and continues shrinking to zero.
func (br *BattleRoyale) currentZone() (cx, cy, radius float64) {
	elapsed := time.Since(br.StartTime)
	mainDur := br.ZoneDuration.Seconds()
	progress := elapsed.Seconds() / mainDur
	if progress > 1 {
		progress = 1
	}

	// Smooth easing (ease-in-out)
	t := progress

	// Linearly interpolate center
	cx = br.InitialX + (br.FinalX-br.InitialX)*t
	cy = br.InitialY + (br.FinalY-br.InitialY)*t

	// Linearly interpolate radius during main phase
	radius = br.InitialRadius + (br.FinalRadius-br.InitialRadius)*t

	// Sudden death: after main duration, keep shrinking from FinalRadius → 0
	if elapsed > br.ZoneDuration {
		overtime := elapsed - br.ZoneDuration
		sdProgress := overtime.Seconds() / br.SuddenDeathDuration.Seconds()
		if sdProgress > 1 {
			sdProgress = 1
		}
		radius = br.FinalRadius * (1 - sdProgress)
		// Center stays at final position during sudden death
		cx = br.FinalX
		cy = br.FinalY
	}

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
