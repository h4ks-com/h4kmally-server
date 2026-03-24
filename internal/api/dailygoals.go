package api

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/h4ks-com/h4kmally-server/internal/game"
)

// ── Goal Types ───────────────────────────────────────────────

// GoalType identifies one of the 7 daily goal kinds.
type GoalType string

const (
	GoalScore       GoalType = "score"        // Reach mass X in a single life
	GoalPlayerKills GoalType = "player_kills" // Eat X player cells
	GoalVirusShoot  GoalType = "virus_shoot"  // Shoot & split X viruses into players
	GoalGamesPlayed GoalType = "games_played" // Play X games
	GoalPacifist    GoalType = "pacifist"     // Reach mass X without splitting
	GoalRevenge     GoalType = "revenge"      // Eat player within X sec of them eating your cell
	GoalMassEjected GoalType = "mass_ejected" // Eject X total mass
)

// GoalDef defines a goal template with its range and scaling.
type GoalDef struct {
	Type        GoalType
	Label       string // human-readable name
	Description string // description template (use %d for target value)
	MinVal      int    // minimum target value
	MaxVal      int    // maximum target value
	IsWindow    bool   // if true, value is a time window (lower = harder)
}

var goalDefs = []GoalDef{
	{
		Type:        GoalScore,
		Label:       "Mass Master",
		Description: "Earn %s total mass across all lives",
		MinVal:      500000,
		MaxVal:      5000000,
	},
	{
		Type:        GoalPlayerKills,
		Label:       "Hunter",
		Description: "Eat %d player cells",
		MinVal:      15,
		MaxVal:      30,
	},
	{
		Type:        GoalVirusShoot,
		Label:       "Virus Sniper",
		Description: "Shoot viruses that split %d players",
		MinVal:      30,
		MaxVal:      100,
	},
	{
		Type:        GoalGamesPlayed,
		Label:       "Dedicated",
		Description: "Play %d games",
		MinVal:      3,
		MaxVal:      8,
	},
	{
		Type:        GoalPacifist,
		Label:       "Pacifist Giant",
		Description: "Reach %s mass without splitting",
		MinVal:      3000,
		MaxVal:      5000,
	},
	{
		Type:        GoalRevenge,
		Label:       "Revenge!",
		Description: "Eat a player within %d sec of them eating your cell",
		MinVal:      5,
		MaxVal:      20,
		IsWindow:    true,
	},
	{
		Type:        GoalMassEjected,
		Label:       "Generous",
		Description: "Eject %s total mass",
		MinVal:      100000,
		MaxVal:      1000000,
	},
}

// ── Daily Goal Instance ─────────────────────────────────────

// DailyGoal is a specific goal assigned to a user for today.
type DailyGoal struct {
	Type        GoalType `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Target      int      `json:"target"`
	Progress    int      `json:"progress"`
	Completed   bool     `json:"completed"`
}

// ── Powerup Types ────────────────────────────────────────────

type PowerupType string

const (
	PowerupVirusLayer     PowerupType = "virus_layer"
	PowerupSpeedBoost     PowerupType = "speed_boost"
	PowerupGhostMode      PowerupType = "ghost_mode"
	PowerupMassMagnet     PowerupType = "mass_magnet"
	PowerupFreezeSplitter PowerupType = "freeze_splitter"
	PowerupRecombine      PowerupType = "recombine"
)

type PowerupDef struct {
	Type        PowerupType `json:"type"`
	Label       string      `json:"label"`
	Description string      `json:"description"`
	Charges     int         `json:"charges"`
	KeySlot     int         `json:"keySlot"` // 1-6
}

var PowerupDefs = []PowerupDef{
	{PowerupVirusLayer, "Virus Layer", "Drop a virus behind your farthest blob", 5, 1},
	{PowerupSpeedBoost, "Speed Boost", "6 seconds of 2x speed", 3, 2},
	{PowerupGhostMode, "Ghost Mode", "Pass through cells for 6s", 1, 3},
	{PowerupMassMagnet, "Mass Magnet", "Pull nearby mass & enemies for 5s", 2, 4},
	{PowerupFreezeSplitter, "Freeze Splitter", "Shoot a virus that splits & freezes an enemy for 3s", 3, 5},
	{PowerupRecombine, "Recombine", "Rapidly merge all your split cells", 1, 6},
}

// ── User Daily State (persisted) ─────────────────────────────

// UserDailyState stores the current daily goals + powerup inventory for a user.
type UserDailyState struct {
	// Date key (e.g. "2026-03-24") — when this state becomes stale, regenerate goals
	DateKey string `json:"dateKey"`

	// 3 daily goals
	Goals [3]DailyGoal `json:"goals"`

	// Whether goals were all completed and a powerup was granted
	PowerupGranted bool `json:"powerupGranted"`

	// Powerup inventory: maps type → remaining charges (multiple stack)
	Powerups map[PowerupType]int `json:"powerups,omitempty"`
}

// ── Goal Generation ──────────────────────────────────────────

// todayKey returns today's date string in UTC.
func todayKey() string {
	return time.Now().UTC().Format("2006-01-02")
}

// deterministicRand returns a deterministic pseudo-random number seeded by user+date.
func deterministicSeed(userSub, dateKey string) uint64 {
	h := sha256.Sum256([]byte(userSub + "|" + dateKey))
	return binary.LittleEndian.Uint64(h[:8])
}

// simpleRand is a minimal xorshift PRNG for seeded repeatable sequences.
type simpleRand struct{ s uint64 }

func (r *simpleRand) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}
func (r *simpleRand) intn(n int) int {
	return int(r.next() % uint64(n))
}

// GenerateDailyGoals creates 3 goals for a user for today, deterministically.
// Ensures variety: at least 1 action goal and 1 accumulation goal.
func GenerateDailyGoals(userSub string, cfg game.Config, botCount int) [3]DailyGoal {
	dateKey := todayKey()
	rng := &simpleRand{deterministicSeed(userSub, dateKey)}

	// Categorize goals
	actionGoals := []int{}  // indices into goalDefs: player_kills, virus_shoot, revenge
	accumGoals := []int{}   // score, pacifist, mass_ejected
	neutralGoals := []int{} // games_played
	for i, d := range goalDefs {
		switch d.Type {
		case GoalPlayerKills, GoalVirusShoot, GoalRevenge:
			actionGoals = append(actionGoals, i)
		case GoalScore, GoalPacifist, GoalMassEjected:
			accumGoals = append(accumGoals, i)
		default:
			neutralGoals = append(neutralGoals, i)
		}
	}

	// Pick 3 unique goals: 1 action, 1 accum, 1 any (from remaining)
	picked := make([]int, 0, 3)

	// 1. Pick one action goal
	idx := rng.intn(len(actionGoals))
	picked = append(picked, actionGoals[idx])

	// 2. Pick one accumulation goal
	idx = rng.intn(len(accumGoals))
	picked = append(picked, accumGoals[idx])

	// 3. Pick one from remaining (any not yet picked)
	remaining := []int{}
	for i := range goalDefs {
		alreadyPicked := false
		for _, p := range picked {
			if p == i {
				alreadyPicked = true
				break
			}
		}
		if !alreadyPicked {
			remaining = append(remaining, i)
		}
	}
	idx = rng.intn(len(remaining))
	picked = append(picked, remaining[idx])

	// Compute scaling factors from config (floor at 0.1 to avoid 0-target goals)
	foodScale := math.Max(0.1, float64(cfg.FoodCount)/2000.0)
	foodSizeScale := math.Max(0.1, cfg.FoodSize/17.3)
	virusScale := math.Max(0.1, float64(cfg.VirusCount)/30.0)
	botScale := math.Max(1.0, 1.0+float64(botCount)/20.0)

	var goals [3]DailyGoal
	for i, defIdx := range picked {
		def := goalDefs[defIdx]

		// Scale the target range
		scale := 1.0
		switch def.Type {
		case GoalScore, GoalMassEjected:
			scale = foodScale
		case GoalPlayerKills:
			scale = botScale
		case GoalVirusShoot:
			scale = virusScale
		case GoalPacifist:
			scale = foodScale * foodSizeScale
		}

		minV := int(float64(def.MinVal) * scale)
		maxV := int(float64(def.MaxVal) * scale)
		if minV > maxV {
			minV, maxV = maxV, minV
		}

		target := minV + rng.intn(maxV-minV+1)
		if target < 1 {
			target = 1
		}

		desc := def.Description
		switch def.Type {
		case GoalScore, GoalPacifist, GoalMassEjected:
			desc = fmt.Sprintf(def.Description, formatMass(target))
		case GoalRevenge:
			desc = fmt.Sprintf(def.Description, target)
		default:
			desc = fmt.Sprintf(def.Description, target)
		}

		goals[i] = DailyGoal{
			Type:        def.Type,
			Label:       def.Label,
			Description: desc,
			Target:      target,
			Progress:    0,
			Completed:   false,
		}
	}

	return goals
}

// formatMass formats a mass value with K/M suffixes.
func formatMass(v int) string {
	if v >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(v)/1000000.0)
	}
	if v >= 1000 {
		return fmt.Sprintf("%.0fK", float64(v)/1000.0)
	}
	return fmt.Sprintf("%d", v)
}

// ── Powerup Granting ─────────────────────────────────────────

// GrantRandomPowerup picks a random powerup for a user.
func GrantRandomPowerup(userSub string) (PowerupType, int) {
	dateKey := todayKey()
	rng := &simpleRand{deterministicSeed(userSub+"_powerup", dateKey)}
	def := PowerupDefs[rng.intn(len(PowerupDefs))]
	return def.Type, def.Charges
}

// GetPowerupDef returns the definition for a given powerup type.
func GetPowerupDef(t PowerupType) *PowerupDef {
	for i := range PowerupDefs {
		if PowerupDefs[i].Type == t {
			return &PowerupDefs[i]
		}
	}
	return nil
}
