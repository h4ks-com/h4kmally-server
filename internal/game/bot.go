package game

import (
	"encoding/json"
	"log"
	"math"
	"math/rand/v2"
	"os"
)

// Bot names — used round-robin or randomly.
var botNames = []string{
	"Bot Alpha", "Bot Bravo", "Bot Charlie", "Bot Delta", "Bot Echo",
	"Bot Foxtrot", "Bot Golf", "Bot Hotel", "Bot India", "Bot Juliet",
	"Bot Kilo", "Bot Lima", "Bot Mike", "Bot November", "Bot Oscar",
	"Bot Papa", "Bot Quebec", "Bot Romeo", "Bot Sierra", "Bot Tango",
	"Bot Uniform", "Bot Victor", "Bot Whiskey", "Bot X-ray", "Bot Yankee",
	"Bot Zulu", "Bot Ace", "Bot Blaze", "Bot Cipher", "Bot Dagger",
	"Bot Ember", "Bot Frost", "Bot Ghost", "Bot Hawk", "Bot Iron",
	"Bot Jade", "Bot Knight", "Bot Luna", "Bot Mist", "Bot Nova",
	"Bot Omega", "Bot Pulse", "Bot Raven", "Bot Storm", "Bot Titan",
	"Bot Viper", "Bot Wolf", "Bot Zen", "Bot Apex", "Bot Comet",
}

// freeSkinNames holds names of free skins loaded from the manifest.
var freeSkinNames []string

func init() {
	data, err := os.ReadFile("skins/manifest.json")
	if err != nil {
		log.Printf("bot: could not load skins manifest: %v", err)
		return
	}
	var entries []struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("bot: could not parse skins manifest: %v", err)
		return
	}
	for _, e := range entries {
		if e.Category == "free" {
			freeSkinNames = append(freeSkinNames, e.Name)
		}
	}
	log.Printf("bot: loaded %d free skins", len(freeSkinNames))
}

// Bot wraps a Player with Ogar-style AI state.
type Bot struct {
	Player *Player

	// AI state
	splitCooldown int // ticks until next split is allowed
}

// NewBot creates a new bot with a random name and random free skin.
func NewBot() *Bot {
	name := botNames[rand.IntN(len(botNames))]
	skin := ""
	if len(freeSkinNames) > 0 {
		skin = freeSkinNames[rand.IntN(len(freeSkinNames))]
	}
	p := NewPlayer(name, skin, "")
	p.IsSubscriber = false
	return &Bot{
		Player: p,
	}
}

// Decide runs one tick of Ogar-style force-field AI.
// It examines all visible cells and computes an influence vector,
// then sets the bot's mouse target accordingly.
// May also queue a split if a kill opportunity is detected.
func (b *Bot) Decide(engine *Engine) {
	p := b.Player
	if !p.Alive || len(p.Cells) == 0 {
		return
	}

	// Decrement split cooldown
	if b.splitCooldown > 0 {
		b.splitCooldown--
	}

	// Get bot's largest cell (primary reference for AI decisions)
	largest := p.LargestCell()
	if largest == nil {
		return
	}
	mySize := largest.Size
	myMass := largest.Mass()

	// Get the bot's center position
	cx, cy := p.Center()

	// Compute viewport for this bot
	vp := ViewportForPlayer(p, engine.Cfg.MapWidth, engine.Cfg.MapHeight)

	// Get all visible cells using the spatial grid
	visible := engine.GetCellsInViewport(vp)

	// Force-field accumulator
	var resultX, resultY float64

	// Track best split target
	var splitTarget *Cell
	splitTargetDist := math.MaxFloat64

	for _, cell := range visible {
		// Skip own cells
		if cell.Owner == p {
			continue
		}

		dx := cell.X - cx
		dy := cell.Y - cy
		dist := math.Sqrt(dx*dx + dy*dy)
		if dist < 1 {
			dist = 1
		}

		// Normalize direction
		nx := dx / dist
		ny := dy / dist

		var influence float64

		switch cell.Type {
		case CellFood:
			// Food: weak attraction
			influence = 1.0

		case CellEject:
			// Ejected mass: moderate attraction if we can eat it
			if myMass > cell.Mass()*1.3 {
				influence = cell.Size // attract proportional to size
			}

		case CellPlayer:
			// Other player cell
			otherMass := cell.Mass()
			edgeDist := dist - mySize - cell.Size // edge-to-edge distance

			if myMass > otherMass*1.3 {
				// We can eat them — strong attraction
				influence = cell.Size * 2.5

				// Check if this is a viable split-kill target
				if b.splitCooldown == 0 && len(p.Cells) < 8 {
					// After split, each half has half mass
					halfMass := myMass / 2.0
					if halfMass > otherMass*1.3 && myMass < otherMass*5.0 {
						// Split range: roughly 4× our size from edge
						splitRange := 400.0 - mySize/2.0 - cell.Size
						if splitRange > 0 && edgeDist <= splitRange && edgeDist > 0 {
							if edgeDist < splitTargetDist {
								splitTarget = cell
								splitTargetDist = edgeDist
							}
						}
					}
				}
			} else if otherMass > myMass*1.3 {
				// They can eat us — flee! Use edge-to-edge distance for threats.
				threatDist := edgeDist
				if threatDist < 1 {
					threatDist = 1
				}
				// Strong repulsion, scaled by threat size
				influence = -cell.Size
				dist = threatDist // use edge distance for influence scaling
			} else {
				// Similar size — mild avoidance
				influence = -(cell.Size / mySize) / 3.0
			}

		case CellVirus:
			if myMass > cell.Mass()*1.3 {
				// We can eat the virus
				if len(p.Cells) >= engine.Cfg.MaxCells {
					// At max cells — safe to eat virus, even beneficial
					influence = cell.Size * 2.5
				} else {
					// Not at max cells — avoid (virus would split us)
					influence = -1.0
				}
			}
			// If we can't eat it, ignore it (viruses don't eat players directly)
		}

		// Apply influence scaled by inverse distance
		influence /= dist
		resultX += nx * influence
		resultY += ny * influence
	}

	// Add border avoidance force
	mapW := engine.Cfg.MapWidth
	mapH := engine.Cfg.MapHeight
	borderMargin := 300.0

	if cx < -mapW+borderMargin {
		resultX += ((-mapW + borderMargin) - cx) / borderMargin
	}
	if cx > mapW-borderMargin {
		resultX += ((mapW - borderMargin) - cx) / borderMargin
	}
	if cy < -mapH+borderMargin {
		resultY += ((-mapH + borderMargin) - cy) / borderMargin
	}
	if cy > mapH-borderMargin {
		resultY += ((mapH - borderMargin) - cy) / borderMargin
	}

	// Normalize result and project mouse target 800 units away
	mag := math.Sqrt(resultX*resultX + resultY*resultY)
	if mag > 0.001 {
		resultX /= mag
		resultY /= mag
	} else {
		// No significant forces — wander randomly
		angle := rand.Float64() * 2 * math.Pi
		resultX = math.Cos(angle)
		resultY = math.Sin(angle)
	}

	mouseX := cx + resultX*800.0
	mouseY := cy + resultY*800.0

	p.SetMouse(mouseX, mouseY)

	// Execute split if we found a valid target
	if splitTarget != nil {
		// Aim at the target before splitting
		p.SetMouse(splitTarget.X, splitTarget.Y)
		p.QueueSplit()
		b.splitCooldown = 15 // 15 ticks cooldown (~600ms at 25Hz)
	}
}
