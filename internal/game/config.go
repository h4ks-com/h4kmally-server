package game

import "time"

// Config holds all tunable game parameters.
type Config struct {
	// Map
	MapWidth  float64 // half-width  (border goes from -MapWidth to +MapWidth)
	MapHeight float64 // half-height

	// Tick
	TickRate time.Duration // server tick interval

	// Player
	StartSize     float64 // initial cell radius on spawn
	MinPlayerSize float64 // minimum cell size (can't shrink below this)
	MinSplitSize  float64 // minimum size to split
	MaxCells      int     // maximum cells per player
	MergeDelay    time.Duration
	SplitSpeed    float64 // impulse speed for split cells
	EjectSize     float64 // radius of ejected mass
	EjectSpeed    float64 // impulse speed for ejected mass
	DecayRate     float64 // per-tick size decay factor (e.g. 0.998 = 0.2% per tick)
	DecayMinSize  float64 // cells below this size don't decay
	MoveSpeed     float64 // base movement speed (scaled by size)
	SplitDistance float64 // how far split cells travel

	// Food
	FoodCount    int     // target number of food pellets on the map
	FoodSize     float64 // food pellet radius (fixed size)
	FoodSpawnPer int     // food spawned per tick (to reach FoodCount)

	// Virus
	VirusCount    int     // target number of viruses on the map
	VirusSize     float64 // virus radius
	VirusMaxSize  float64 // max virus size before it splits
	VirusFeedSize float64 // size increase per W mass absorbed
	VirusSplit    int     // how many pieces a player splits into when hitting a virus

	// Leaderboard
	LeaderboardSize int           // number of entries
	LeaderboardRate time.Duration // how often to broadcast
}

// DefaultConfig returns a configuration with default gameplay values.
func DefaultConfig() Config {
	return Config{
		MapWidth:  7071,
		MapHeight: 7071,

		TickRate: 40 * time.Millisecond, // 25 Hz

		StartSize:     100,
		MinPlayerSize: 100,
		MinSplitSize:  122.5,
		MaxCells:      16,
		MergeDelay:    30 * time.Second,
		SplitSpeed:    1045,
		EjectSize:     36.06,
		EjectSpeed:    1045,
		DecayRate:     0.9998,
		DecayMinSize:  100,
		MoveSpeed:     2.0,
		SplitDistance: 951,

		FoodCount:    2000,
		FoodSize:     17.3,
		FoodSpawnPer: 10,

		VirusCount:    30,
		VirusSize:     141.4,
		VirusMaxSize:  288.4,
		VirusFeedSize: 36.06, // matches eject size; 7 feeds to split
		VirusSplit:    10,

		LeaderboardSize: 10,
		LeaderboardRate: 2 * time.Second,
	}
}
