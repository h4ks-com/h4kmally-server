package game

import (
	"log"
)

// BotManager manages server-side bots.
type BotManager struct {
	engine *Engine
	bots   []*Bot
	count  int // desired bot count
}

// NewBotManager creates a bot manager that maintains the given number of bots.
func NewBotManager(engine *Engine, count int) *BotManager {
	bm := &BotManager{
		engine: engine,
		bots:   make([]*Bot, 0, count),
		count:  count,
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
	for _, bot := range bm.bots {
		if !bot.Player.Alive {
			// Auto-respawn dead bots immediately
			bm.engine.SpawnPlayer(bot.Player)
		}
		bot.Decide(bm.engine)
	}
}

// BotCount returns the number of bots currently managed.
func (bm *BotManager) BotCount() int {
	return len(bm.bots)
}
