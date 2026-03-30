package game

import (
	"log"
)

// BotManager manages server-side bots.
type BotManager struct {
	engine *Engine
	bots   []*Bot
	count  int // desired bot count
	br     *BattleRoyale
}

// NewBotManager creates a bot manager that maintains the given number of bots.
func NewBotManager(engine *Engine, count int, br *BattleRoyale) *BotManager {
	bm := &BotManager{
		engine: engine,
		bots:   make([]*Bot, 0, count),
		count:  count,
		br:     br,
	}
	// Spawn initial bots
	for i := 0; i < count; i++ {
		bm.addBot()
	}
	log.Printf("BotManager: spawned %d bots", count)
	return bm
}

// addBot creates a new bot, adds it to the engine, and spawns it.
func (bm *BotManager) addBot() {
	bot := NewBot()
	bm.engine.AddPlayer(bot.Player)
	bm.engine.SpawnPlayer(bot.Player)
	bm.bots = append(bm.bots, bot)
}

// Tick runs AI for all bots and respawns dead ones.
// Must be called AFTER engine.Tick() so bots see the latest game state,
// but their actions will be processed on the next engine tick.
func (bm *BotManager) Tick() {
	// Don't respawn bots during active BR (they must stay dead until it ends).
	brActive := bm.br != nil && bm.br.IsActive()

	// Get zone info for bot AI (if BR is active)
	var zoneCX, zoneCY, zoneRadius float64
	var hasZone bool
	if bm.br != nil {
		cx, cy, r, state := bm.br.GetZone()
		if state == BRActive {
			zoneCX, zoneCY, zoneRadius = cx, cy, r
			hasZone = true
		}
	}

	for _, bot := range bm.bots {
		if !bot.Player.Alive {
			if !brActive {
				// Auto-respawn dead bots (only outside of BR)
				bm.engine.SpawnPlayer(bot.Player)
			} else {
				continue // dead in BR = stay dead
			}
		}
		if hasZone {
			bot.DecideBR(bm.engine, zoneCX, zoneCY, zoneRadius)
		} else {
			bot.Decide(bm.engine)
		}
	}
}

// BotCount returns the number of bots currently managed.
func (bm *BotManager) BotCount() int {
	return len(bm.bots)
}

// SetCount dynamically adjusts the target bot count. Adds or removes bots accordingly.
func (bm *BotManager) SetCount(n int) {
	if n < 0 {
		n = 0
	}
	bm.count = n
	// Add bots if needed
	for len(bm.bots) < n {
		bm.addBot()
	}
	// Remove excess bots
	for len(bm.bots) > n {
		last := bm.bots[len(bm.bots)-1]
		bm.engine.RemovePlayer(last.Player.ID)
		bm.bots = bm.bots[:len(bm.bots)-1]
	}
	log.Printf("BotManager: adjusted count to %d", n)
}

// GetBotList returns a snapshot of all bot info for the admin API.
func (bm *BotManager) GetBotList() []BotInfo {
	out := make([]BotInfo, 0, len(bm.bots))
	for _, b := range bm.bots {
		mass := 0.0
		for _, c := range b.Player.Cells {
			mass += c.Size * c.Size / 100
		}
		out = append(out, BotInfo{
			ID:     b.Player.ID,
			Name:   b.Player.Name,
			Skin:   b.Player.Skin,
			Effect: b.Player.Effect,
			Alive:  b.Player.Alive,
			Mass:   int(mass),
		})
	}
	return out
}

// BotInfo is a serializable snapshot of one bot.
type BotInfo struct {
	ID     uint32 `json:"id"`
	Name   string `json:"name"`
	Skin   string `json:"skin"`
	Effect string `json:"effect"`
	Alive  bool   `json:"alive"`
	Mass   int    `json:"mass"`
}
