package api

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/h4kmally-server/internal/game"
	"github.com/h4ks-com/h4kmally-server/internal/protocol"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // allow all origins for development
	},
}

// Client represents a connected WebSocket client.
type Client struct {
	conn    *websocket.Conn
	shuffle *protocol.ShuffleTable
	player  *game.Player
	engine  *game.Engine
	server  *Server

	send chan []byte // buffered channel for outgoing messages

	mu            sync.Mutex
	authenticated bool
	alive         bool

	// Authenticated user sub (from Logto session, empty if guest)
	userSub string

	// Remote IP address (for IP bans)
	remoteIP string

	// Live score tracking: tracks the score already banked to Points
	lastTickScore int64

	// Spectator mode
	spectating         bool
	spectateTarget     uint32 // player ID to follow
	spectatorX         float64
	spectatorY         float64
	spectatorMouseX    float64
	spectatorMouseY    float64
	spectatorFollowing bool // true = auto-follow target, false = free-roam
	godMode            bool // admin-only: see entire map

	// Viewport tracking: set of cell IDs this client currently knows about.
	// When a cell leaves the viewport, we send a removal so the client drops it.
	knownCells   map[uint32]bool
	knownMyCells map[uint32]bool // cell IDs for which we've sent AddMyCell
}

// Server manages all connected clients and the game engine.
type Server struct {
	Engine  *game.Engine
	AuthMgr *AuthManager
	clients map[*Client]bool
	mu      sync.RWMutex
}

// NewServer creates a new game server.
func NewServer(engine *game.Engine) *Server {
	return &Server{
		Engine:  engine,
		clients: make(map[*Client]bool),
	}
}

// HandleWS is the WebSocket endpoint handler (/ws/).
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Extract remote IP
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	// Check forwarded headers
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		remoteIP = fwd
	}

	// Check IP ban
	if s.AuthMgr != nil {
		if banned, reason := s.AuthMgr.UserStore.IsIPBanned(remoteIP); banned {
			log.Printf("Rejected banned IP %s: %s", remoteIP, reason)
			http.Error(w, "Banned: "+reason, 403)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	// Check for session token to link to authenticated user
	var userSub string
	sessionToken := r.URL.Query().Get("session")
	if sessionToken != "" && s.AuthMgr != nil {
		session := s.AuthMgr.ValidateSession(sessionToken)
		if session != nil {
			// Check account ban
			user := s.AuthMgr.UserStore.Get(session.UserSub)
			if user != nil && user.IsBanned() {
				log.Printf("Rejected banned user %s", session.UserSub)
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "Banned: "+user.BanReason))
				conn.Close()
				return
			}
			userSub = session.UserSub
			log.Printf("WebSocket connected with authenticated user: %s", userSub)
		}
	}

	client := &Client{
		conn:         conn,
		server:       s,
		engine:       s.Engine,
		send:         make(chan []byte, 256),
		knownCells:   make(map[uint32]bool, 256),
		knownMyCells: make(map[uint32]bool, 16),
		userSub:      userSub,
		remoteIP:     remoteIP,
	}

	// One connection per account: kick any existing connection from the
	// same authenticated user. Multiple guests / different users on the
	// same IP are allowed.
	s.mu.Lock()
	if userSub != "" {
		for existing := range s.clients {
			if existing.userSub != userSub {
				continue
			}
			log.Printf("Kicking duplicate connection for user %q from IP %s",
				userSub, existing.remoteIP)
			go func(old *Client) {
				old.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "Duplicate connection"))
				old.conn.Close()
			}(existing)
		}
	}
	s.clients[client] = true
	s.mu.Unlock()

	// Start writer goroutine
	go client.writePump()

	// Handle connection in this goroutine
	client.handleConnection()
}

func (c *Client) handleConnection() {
	defer func() {
		c.server.mu.Lock()
		delete(c.server.clients, c)
		c.server.mu.Unlock()

		if c.player != nil {
			c.engine.RemovePlayer(c.player.ID)
			log.Printf("Player %q (ID %d) disconnected", c.player.Name, c.player.ID)
		}
		close(c.send)
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// === Step 1: Wait for client handshake ===
	_, msg, err := c.conn.ReadMessage()
	if err != nil {
		log.Printf("Handshake read error: %v", err)
		return
	}

	// Verify protocol version
	version, _ := protocol.DecodeStringUTF8(msg, 0)
	if version != protocol.ProtocolVersion {
		log.Printf("Bad protocol version: %q", version)
		return
	}

	// === Step 2: Send handshake response (version + shuffle table) ===
	c.shuffle = protocol.NewShuffleTable()
	handshake := protocol.BuildHandshake(c.shuffle)
	if err := c.conn.WriteMessage(websocket.BinaryMessage, handshake); err != nil {
		log.Printf("Handshake write error: %v", err)
		return
	}

	// === Step 3: Send initial border ===
	cfg := c.engine.Cfg
	border := protocol.BuildBorder(c.shuffle, -cfg.MapWidth, -cfg.MapHeight, cfg.MapWidth, cfg.MapHeight)
	// Append extra byte to make byteLength > 33 (triggers ping loop on client)
	borderExt := make([]byte, 34)
	copy(borderExt, border)
	borderExt[33] = 0x01
	c.sendMsg(borderExt)

	log.Printf("Client connected, handshake complete")

	// === Step 4: Read loop ===
	for {
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		parsed := protocol.ParseClientMessage(c.shuffle, raw)
		if parsed == nil {
			continue
		}

		c.handleMessage(parsed)
	}
}

func (c *Client) handleMessage(msg *protocol.ParsedMessage) {
	switch msg.Op {
	case protocol.OpCaptchaToken:
		// Accept any token (we're our own server)
		c.mu.Lock()
		c.authenticated = true
		c.mu.Unlock()

	case protocol.OpSpawn:
		sp, err := protocol.ParseSpawn(msg.Payload)
		if err != nil {
			log.Printf("Bad spawn payload: %v", err)
			c.sendMsg(protocol.BuildSpawnResult(c.shuffle, false))
			return
		}

		// Validate skin access server-side
		validatedSkin := c.validateSkinAccess(sp.Skin)

		// Spawn-time dedup: if another client with the same account has an
		// active player, remove it so we don't end up with ghost duplicates.
		if c.userSub != "" {
			c.server.mu.RLock()
			for other := range c.server.clients {
				if other == c || other.player == nil {
					continue
				}
				if other.userSub == c.userSub {
					log.Printf("Spawn dedup: removing ghost player %q (ID %d) from old connection",
						other.player.Name, other.player.ID)
					c.engine.RemovePlayer(other.player.ID)
					other.player = nil
				}
			}
			c.server.mu.RUnlock()
		}

		// Create or respawn
		if c.player == nil {
			c.player = game.NewPlayer(sp.Name, validatedSkin)
			c.player.Conn = c
			c.engine.AddPlayer(c.player)
			log.Printf("Player %q (ID %d) joined", sp.Name, c.player.ID)
		} else {
			c.player.Name = sp.Name
			c.player.Skin = validatedSkin
		}

		c.engine.SpawnPlayer(c.player)

		// Send spawn result
		c.sendMsg(protocol.BuildSpawnResult(c.shuffle, true))

		// Send ADD_MY_CELL for each cell
		newMyCells := make(map[uint32]bool, len(c.player.Cells))
		for _, cell := range c.player.Cells {
			c.sendMsg(protocol.BuildAddMyCell(c.shuffle, cell.ID))
			newMyCells[cell.ID] = true
		}

		// Send full world sync — all cells on the map
		allCells := c.engine.AllCells()
		syncBuilder := protocol.NewWorldUpdateBuilder(c.shuffle, nil)
		newKnown := make(map[uint32]bool, len(allCells))
		for _, cell := range allCells {
			syncBuilder.AddCell(cell)
			newKnown[cell.ID] = true
		}
		c.sendMsg(syncBuilder.Finish(nil))

		c.mu.Lock()
		c.knownCells = newKnown
		c.knownMyCells = newMyCells
		c.alive = true
		c.mu.Unlock()

	case protocol.OpMouse:
		x, y, ok := protocol.ParseMouse(msg.Payload)
		if !ok {
			return
		}
		if c.player != nil {
			c.player.SetMouse(float64(x), float64(y))
		}
		c.mu.Lock()
		if c.spectating {
			c.spectatorMouseX = float64(x)
			c.spectatorMouseY = float64(y)
			c.spectatorFollowing = false // mouse movement exits follow mode
		}
		c.mu.Unlock()

	case protocol.OpSplit:
		if c.player != nil && c.player.Alive {
			c.player.QueueSplit()
		}

	case protocol.OpEject:
		if c.player != nil && c.player.Alive {
			c.player.QueueEject()
		}

	case protocol.OpChat:
		flags, text, ok := protocol.ParseChat(msg.Payload)
		if !ok || c.player == nil || text == "" {
			return
		}
		_ = flags
		// Broadcast chat to all clients
		c.server.BroadcastChat(c.player, text, nil)

	case protocol.OpPing:
		c.sendMsg(protocol.BuildPingReply(c.shuffle))

	case protocol.OpSpectate:
		c.mu.Lock()
		c.spectating = true
		c.alive = false
		c.spectatorFollowing = true // start in follow mode
		c.spectatorX = 0
		c.spectatorY = 0
		c.mu.Unlock()
		// Send full world sync for initial spectator view
		allCells := c.engine.AllCells()
		syncBuilder := protocol.NewWorldUpdateBuilder(c.shuffle, nil)
		newKnown := make(map[uint32]bool, len(allCells))
		for _, cell := range allCells {
			syncBuilder.AddCell(cell)
			newKnown[cell.ID] = true
		}
		c.sendMsg(syncBuilder.Finish(nil))
		c.mu.Lock()
		c.knownCells = newKnown
		c.mu.Unlock()

	case protocol.OpStatUpdate:
		// Keepalive acknowledged, reset deadline
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	case protocol.OpBoostCheck:
		// Spectator commands: 0x01 = toggle follow, 0x02 = toggle god mode
		if len(msg.Payload) >= 1 {
			cmd := msg.Payload[0]
			c.mu.Lock()
			switch cmd {
			case 0x01:
				c.spectatorFollowing = !c.spectatorFollowing
				if c.spectatorFollowing {
					c.spectateTarget = 0 // reset to auto-follow top player
				}
			case 0x02:
				if c.server.AuthMgr != nil && c.userSub != "" {
					user := c.server.AuthMgr.UserStore.Get(c.userSub)
					if user != nil && user.IsAdmin {
						c.godMode = !c.godMode
					}
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) sendMsg(msg []byte) {
	select {
	case c.send <- msg:
	default:
		// Channel full — client is too slow, drop
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Broadcast sends a viewport-culled world update to each connected client.
// Each tick we scan ALL cells in the client's viewport to discover new ones,
// and compare against knownCells to detect cells that left the viewport.
func (s *Server) Broadcast(updated []*game.Cell, eaten []game.EatEvent, removed []uint32) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cfg := s.Engine.Cfg

	// Build set of updated cell IDs for quick lookup (cells that moved/born)
	updatedSet := make(map[uint32]bool, len(updated))
	for _, c := range updated {
		updatedSet[c.ID] = true
	}

	for client := range s.clients {
		if client.shuffle == nil {
			continue
		}

		client.mu.Lock()
		isSpectating := client.spectating
		spectateTarget := client.spectateTarget
		spectatorFollowing := client.spectatorFollowing
		godMode := client.godMode
		spectatorX := client.spectatorX
		spectatorY := client.spectatorY
		spectatorMouseX := client.spectatorMouseX
		spectatorMouseY := client.spectatorMouseY
		client.mu.Unlock()

		// Determine viewport
		var vp game.Viewport
		var camX, camY float64
		if isSpectating {
			if godMode {
				// Admin god mode: see entire map
				vp = game.Viewport{Left: -cfg.MapWidth, Top: -cfg.MapHeight, Right: cfg.MapWidth, Bottom: cfg.MapHeight}
				camX = spectatorX
				camY = spectatorY
			} else if spectatorFollowing {
				// Follow mode: track the target player
				targetPlayer := s.Engine.GetPlayer(spectateTarget)
				if targetPlayer == nil || !targetPlayer.Alive {
					players := s.Engine.GetPlayers()
					var best *game.Player
					for _, p := range players {
						if p.Alive && (best == nil || p.Score > best.Score) {
							best = p
						}
					}
					if best != nil {
						targetPlayer = best
						client.mu.Lock()
						client.spectateTarget = best.ID
						client.mu.Unlock()
					}
				}
				vp = game.ViewportForPlayer(targetPlayer, cfg.MapWidth, cfg.MapHeight)
				if targetPlayer != nil && targetPlayer.Alive {
					camX, camY = targetPlayer.Center()
					// Keep spectator position in sync for smooth transition
					client.mu.Lock()
					client.spectatorX = camX
					client.spectatorY = camY
					client.mu.Unlock()
				}
			} else {
				// Free-roam mode: move spectator toward mouse
				speed := game.SpeedForSize(cfg.StartSize) * cfg.MoveSpeed * 40
				dx := spectatorMouseX - spectatorX
				dy := spectatorMouseY - spectatorY
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > 1 {
					if speed > dist {
						speed = dist
					}
					spectatorX += (dx / dist) * speed
					spectatorY += (dy / dist) * speed
					// Clamp to map bounds
					if spectatorX < -cfg.MapWidth {
						spectatorX = -cfg.MapWidth
					}
					if spectatorX > cfg.MapWidth {
						spectatorX = cfg.MapWidth
					}
					if spectatorY < -cfg.MapHeight {
						spectatorY = -cfg.MapHeight
					}
					if spectatorY > cfg.MapHeight {
						spectatorY = cfg.MapHeight
					}
					client.mu.Lock()
					client.spectatorX = spectatorX
					client.spectatorY = spectatorY
					client.mu.Unlock()
				}
				vp = game.ViewportForSpectator(spectatorX, spectatorY, cfg.MapWidth, cfg.MapHeight)
				camX = spectatorX
				camY = spectatorY
			}
		} else {
			vp = game.ViewportForPlayer(client.player, cfg.MapWidth, cfg.MapHeight)
		}

		// Get ALL cells currently in this client's viewport (scans entire cell map)
		visible := s.Engine.GetCellsInViewport(vp)

		// Always include the player's own cells even if outside viewport
		if client.player != nil && client.player.Alive && !isSpectating {
			visibleOwnSet := make(map[uint32]bool, len(visible))
			for _, c := range visible {
				visibleOwnSet[c.ID] = true
			}
			for _, c := range client.player.Cells {
				if !visibleOwnSet[c.ID] {
					visible = append(visible, c)
				}
			}
		}

		// Build set of visible cell IDs
		visibleSet := make(map[uint32]bool, len(visible))
		for _, c := range visible {
			visibleSet[c.ID] = true
		}

		// Lock client mutex while reading/writing knownCells
		client.mu.Lock()

		builder := protocol.NewWorldUpdateBuilder(client.shuffle, eaten)

		// For each cell in the viewport:
		// - If client doesn't know about it yet → send it (new to viewport)
		// - If client knows it AND it was updated this tick → send update
		for _, c := range visible {
			if !client.knownCells[c.ID] {
				builder.AddCell(c)
				client.knownCells[c.ID] = true
			} else if updatedSet[c.ID] {
				builder.AddCell(c)
			}
		}

		// Build removal list:
		// - Cells that were removed/eaten server-side
		// - Cells the client knows about that are no longer in the viewport
		var clientRemoved []uint32
		for _, id := range removed {
			if client.knownCells[id] {
				clientRemoved = append(clientRemoved, id)
				delete(client.knownCells, id)
			}
		}
		for id := range client.knownCells {
			if !visibleSet[id] {
				clientRemoved = append(clientRemoved, id)
				delete(client.knownCells, id)
			}
		}

		client.mu.Unlock()

		msg := builder.Finish(clientRemoved)
		client.sendMsg(msg)

		// Send AddMyCell for any new cells the player gained (split, etc.)
		if client.player != nil && client.player.Alive && !isSpectating {
			for _, cell := range client.player.Cells {
				if !client.knownMyCells[cell.ID] {
					client.sendMsg(protocol.BuildAddMyCell(client.shuffle, cell.ID))
					client.knownMyCells[cell.ID] = true
				}
			}
		}

		// Send camera update
		if isSpectating {
			cam := protocol.BuildCamera(client.shuffle, float32(camX), float32(camY))
			client.sendMsg(cam)
		} else if client.player != nil && client.player.Alive {
			cx, cy := client.player.Center()
			cam := protocol.BuildCamera(client.shuffle, float32(cx), float32(cy))
			client.sendMsg(cam)
		}

		// Update live points for authenticated alive players
		if client.player != nil && client.player.Alive && client.userSub != "" && s.AuthMgr != nil {
			score := int64(client.player.Score)
			delta := score - client.lastTickScore
			if delta > 0 {
				s.AuthMgr.UserStore.UpdatePoints(client.userSub, delta, score)
				client.lastTickScore = score
			}
		}

		// Check if player died
		client.mu.Lock()
		wasAlive := client.alive
		client.mu.Unlock()
		if client.player != nil && wasAlive && !client.player.Alive {
			// Bank any remaining score delta and record the game
			if client.userSub != "" && s.AuthMgr != nil {
				score := int64(client.player.Score)
				remainingDelta := score - client.lastTickScore
				if remainingDelta > 0 {
					s.AuthMgr.UserStore.UpdatePoints(client.userSub, remainingDelta, score)
				}
				s.AuthMgr.UserStore.RecordGame(client.userSub)
				log.Printf("Recorded game for user %s (score %d)", client.userSub, score)
			}
			client.lastTickScore = 0

			client.sendMsg(protocol.BuildClearMine(client.shuffle))
			client.mu.Lock()
			client.alive = false
			client.knownMyCells = make(map[uint32]bool, 16)
			client.mu.Unlock()
		}
	}
}

// BroadcastLeaderboard sends the leaderboard to all clients.
func (s *Server) BroadcastLeaderboard(entries []game.LeaderEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for client := range s.clients {
		if client.shuffle == nil {
			continue
		}
		name := ""
		if client.player != nil {
			name = client.player.Name
		}
		msg := protocol.BuildLeaderboardFFA(client.shuffle, entries, name)
		client.sendMsg(msg)
	}
}

// BroadcastChat sends a chat message to all clients (each with their own shuffle).
func (s *Server) BroadcastChat(sender *game.Player, text string, _ []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for client := range s.clients {
		if client.shuffle == nil {
			continue
		}
		msg := protocol.BuildChat(client.shuffle, 0, sender.Color[0], sender.Color[1], sender.Color[2], sender.Name, text)
		client.sendMsg(msg)
	}
}

// KickUserSub disconnects all clients associated with a user sub.
func (s *Server) KickUserSub(sub string, reason string) {
	s.mu.RLock()
	var toKick []*Client
	for client := range s.clients {
		if client.userSub == sub {
			toKick = append(toKick, client)
		}
	}
	s.mu.RUnlock()

	for _, client := range toKick {
		client.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason))
		client.conn.Close()
	}
}

// KickIP disconnects all clients from a given IP address.
func (s *Server) KickIP(ip string, reason string) {
	s.mu.RLock()
	var toKick []*Client
	for client := range s.clients {
		if client.remoteIP == ip {
			toKick = append(toKick, client)
		}
	}
	s.mu.RUnlock()

	for _, client := range toKick {
		client.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason))
		client.conn.Close()
	}
}

// SaveUserPoints persists user store to disk (called periodically from tick loop).
func (s *Server) SaveUserPoints() {
	if s.AuthMgr != nil {
		s.AuthMgr.UserStore.SaveAll()
	}
}

// HandleRecaptcha is a dummy endpoint that always succeeds.
func HandleRecaptcha(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

// HandleAuth is a dummy auth endpoint.
func HandleAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"result": "success",
		"body": map[string]interface{}{
			"user": map[string]interface{}{
				"token": "local-dev-token",
				"email": "dev@local",
			},
		},
	})
}

// validateSkinAccess checks if this client is allowed to use the requested skin.
// Returns the skin name if allowed, or "" if not.
func (c *Client) validateSkinAccess(skinName string) string {
	if skinName == "" {
		return ""
	}

	// Load manifest to find the skin
	skins, err := loadManifest()
	if err != nil {
		return "" // can't validate, deny
	}

	var found *skinEntry
	for i := range skins {
		if skins[i].Name == skinName {
			found = &skins[i]
			break
		}
	}
	if found == nil {
		return "" // skin doesn't exist
	}

	// Free skins are available to everyone (including guests/bots)
	if found.Category == "free" {
		return skinName
	}

	// Guest (no user sub) — cannot use non-free skins
	if c.userSub == "" {
		return ""
	}

	// Get user profile
	if c.server.AuthMgr == nil {
		return skinName // no auth system, allow
	}
	user := c.server.AuthMgr.UserStore.Get(c.userSub)
	if user == nil {
		return ""
	}

	// Admin can use any skin
	if user.IsAdmin {
		return skinName
	}

	switch found.Category {
	case "level":
		userLevel := LevelFromPoints(user.Points)
		if found.MinLevel > 0 && userLevel < found.MinLevel {
			return "" // not high enough level
		}
		return skinName
	case "premium":
		if skinInSlice(user.UnlockedSkins, skinName) {
			return skinName
		}
		return "" // not unlocked
	default:
		return "" // unknown category
	}
}

// HandleSkinsList serves the skins manifest JSON from disk.
func HandleSkinsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, "skins/manifest.json")
}

// SkinAccessEntry is one skin with access info for a specific user.
type SkinAccessEntry struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Category   string `json:"category"`
	Rarity     string `json:"rarity"`
	MinLevel   int    `json:"minLevel,omitempty"`
	Accessible bool   `json:"accessible"`           // can the user equip this skin?
	Reason     string `json:"reason,omitempty"`     // why not accessible (for UI)
	Tokens     int    `json:"tokens,omitempty"`     // current tokens for premium skins
	TokensNeed int    `json:"tokensNeed,omitempty"` // tokens needed to unlock
}

// HandleSkinsAccess returns all skins with per-user access info.
// GET /api/skins/access?session=TOKEN
func (s *Server) HandleSkinsAccess(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	// Load manifest
	skins, err := loadManifest()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to load skins"})
		return
	}

	// Check if user is authenticated
	var user *UserProfile
	var isAdmin bool
	sessionToken := r.URL.Query().Get("session")
	if sessionToken != "" && s.AuthMgr != nil {
		session := s.AuthMgr.ValidateSession(sessionToken)
		if session != nil {
			user = s.AuthMgr.UserStore.Get(session.UserSub)
			if user != nil {
				isAdmin = user.IsAdmin
			}
		}
	}

	result := make([]SkinAccessEntry, 0, len(skins))
	for _, sk := range skins {
		entry := SkinAccessEntry{
			Name:     sk.Name,
			File:     sk.File,
			Category: sk.Category,
			Rarity:   sk.Rarity,
			MinLevel: sk.MinLevel,
		}

		if user == nil {
			// Guest: can see but not use any skin
			entry.Accessible = false
			entry.Reason = "Sign in to use skins"
		} else if isAdmin {
			// Admin: can use any skin
			entry.Accessible = true
		} else {
			switch sk.Category {
			case "free":
				entry.Accessible = true
			case "level":
				userLevel := LevelFromPoints(user.Points)
				if sk.MinLevel > 0 && userLevel < sk.MinLevel {
					entry.Accessible = false
					entry.Reason = "Requires level " + json.Number(fmt.Sprintf("%d", sk.MinLevel)).String()
				} else {
					entry.Accessible = true
				}
			case "premium":
				tokens := 0
				if user.SkinTokens != nil {
					tokens = user.SkinTokens[sk.Name]
				}
				entry.Tokens = tokens
				entry.TokensNeed = TokensPerSkinUnlock
				if skinInSlice(user.UnlockedSkins, sk.Name) {
					entry.Accessible = true
				} else {
					entry.Accessible = false
					entry.Reason = fmt.Sprintf("%d/%d tokens", tokens, TokensPerSkinUnlock)
				}
			default:
				// clan or other categories — locked for now
				entry.Accessible = false
				entry.Reason = "Not available"
			}
		}

		result = append(result, entry)
	}

	// Also return user level and pending tokens for the client
	resp := map[string]interface{}{
		"skins": result,
	}
	if user != nil {
		resp["level"] = LevelFromPoints(user.Points)
		resp["pendingTokens"] = user.PendingTokens
	}

	json.NewEncoder(w).Encode(resp)
}
