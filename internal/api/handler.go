package api

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"runtime"
	"sync"
	sync_atomic "sync/atomic"
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

	// Viewport tracking: flat arrays indexed by cell ID for O(1) lookups.
	// knownSet[id]=true means this client has been told about cell #id.
	// knownIDs is the list of known cell IDs (for iteration / exit checks).
	knownSet     []bool
	knownIDs     []uint32
	knownMyCells map[uint32]bool // cell IDs for which we've sent AddMyCell

	// Multibox: second player slot
	multiEnabled    bool            // multibox feature active
	multiPlayer     *game.Player    // the second cell group (nil if not enabled)
	multiAlive      bool            // is the multi player alive
	multiSlot       byte            // 0 = controlling primary, 1 = controlling multi
	knownMultiCells map[uint32]bool // cell IDs for which we've sent AddMultiCell
	multiRespawn    int64           // unix time to auto-respawn multi (0 = not pending)
	primaryRespawn  int64           // unix time to auto-respawn primary (0 = not pending)

	// Inactivity tracking: last time any message was received from this client.
	// Accessed atomically (unix nanoseconds). If no activity for 10s, the
	// server force-disconnects the client to prevent ghost cells.
	lastActivity int64 // atomic: time.Now().UnixNano()
}

// Server manages all connected clients and the game engine.
type Server struct {
	Engine       *game.Engine
	AuthMgr      *AuthManager
	ChatBridge   *ChatBridge
	ClanStore    *ClanStore
	BattleRoyale *game.BattleRoyale
	clients      map[*Client]bool
	mu           sync.RWMutex
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
		knownSet:     make([]bool, game.MaxCellID()+256),
		knownIDs:     make([]uint32, 0, 256),
		knownMyCells: make(map[uint32]bool, 16),
		userSub:      userSub,
		remoteIP:     remoteIP,
		lastActivity: time.Now().UnixNano(),
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
			c.engine.QueueRemovePlayer(c.player.ID)
			log.Printf("Player %q (ID %d) disconnected", c.player.Name, c.player.ID)
		}
		// Clean up multibox player
		c.mu.Lock()
		mp := c.multiPlayer
		c.multiPlayer = nil
		c.multiEnabled = false
		c.mu.Unlock()
		if mp != nil {
			c.engine.QueueRemovePlayer(mp.ID)
		}

		close(c.send)
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		sync_atomic.StoreInt64(&c.lastActivity, time.Now().UnixNano())
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

		// Track last activity for inactivity failsafe.
		sync_atomic.StoreInt64(&c.lastActivity, time.Now().UnixNano())

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

		// Block spawning during an active Battle Royale (countdown or active).
		// Once you die in BR, you stay dead until the round ends.
		if c.server.BattleRoyale != nil && c.server.BattleRoyale.IsActive() {
			c.sendMsg(protocol.BuildSpawnResult(c.shuffle, false))
			return
		}

		// Validate skin access server-side
		validatedSkin := c.validateSkinAccess(sp.Skin)
		validatedEffect := c.validateEffect(sp.Effect)

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
					c.engine.QueueRemovePlayer(other.player.ID)
					other.player = nil
					// Also remove the old connection's multibox player
					other.mu.Lock()
					if mp := other.multiPlayer; mp != nil {
						c.engine.QueueRemovePlayer(mp.ID)
						other.multiPlayer = nil
						other.multiEnabled = false
						other.multiAlive = false
					}
					other.mu.Unlock()
				}
			}
			c.server.mu.RUnlock()
		}

		// Create or respawn
		if c.player == nil {
			c.player = game.NewPlayer(sp.Name, validatedSkin, validatedEffect)
			c.player.Conn = c
			c.engine.AddPlayer(c.player)
			log.Printf("Player %q (ID %d) joined", sp.Name, c.player.ID)
		} else {
			c.player.Name = sp.Name
			c.player.Skin = validatedSkin
			c.player.Effect = validatedEffect
		}

		// Look up clan tag for authenticated users
		if c.userSub != "" && c.server.AuthMgr != nil && c.server.ClanStore != nil {
			if user := c.server.AuthMgr.UserStore.Get(c.userSub); user != nil && user.ClanID != "" {
				if clan := c.server.ClanStore.GetClan(user.ClanID); clan != nil {
					c.player.Clan = clan.Tag
				} else {
					c.player.Clan = ""
				}
			} else {
				c.player.Clan = ""
			}
		}

		c.engine.SpawnPlayer(c.player)

		// If multibox is enabled and the multi player is alive, teleport
		// the freshly-spawned primary next to the multi player so they
		// stay together after death.
		c.mu.Lock()
		if c.multiEnabled && c.multiAlive && c.multiPlayer != nil && c.multiPlayer.Alive {
			mx, my := c.multiPlayer.Center()
			c.engine.SpawnPlayerNear(c.player, mx, my, 500)
		}
		c.mu.Unlock()

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
		maxID := game.MaxCellID()
		newSet := make([]bool, maxID+256)
		newIDs := make([]uint32, 0, len(allCells))
		for _, cell := range allCells {
			syncBuilder.AddCell(cell)
			if int(cell.ID) < len(newSet) {
				newSet[cell.ID] = true
			}
			newIDs = append(newIDs, cell.ID)
		}
		c.sendMsg(syncBuilder.Finish(nil))

		c.mu.Lock()
		c.knownSet = newSet
		c.knownIDs = newIDs
		c.knownMyCells = newMyCells
		c.alive = true
		c.spectating = false
		c.godMode = false
		c.mu.Unlock()

	case protocol.OpMouse:
		x, y, ok := protocol.ParseMouse(msg.Payload)
		if !ok {
			return
		}
		// Route mouse to the active multibox slot
		activePlayer := c.activePlayer()
		if activePlayer != nil {
			activePlayer.SetMouse(float64(x), float64(y))
		}
		c.mu.Lock()
		if c.spectating {
			c.spectatorMouseX = float64(x)
			c.spectatorMouseY = float64(y)
			c.spectatorFollowing = false // mouse movement exits follow mode
		}
		c.mu.Unlock()

	case protocol.OpSplit:
		p := c.activePlayer()
		if p != nil && p.Alive {
			p.QueueSplit()
		}

	case protocol.OpEject:
		p := c.activePlayer()
		if p != nil && p.Alive {
			p.QueueEject()
		}

	case protocol.OpMultiboxToggle:
		c.handleMultiboxToggle()

	case protocol.OpMultiboxSwitch:
		c.handleMultiboxSwitch()

	case protocol.OpDirectionLock:
		c.handleDirectionLock(msg.Payload)

	case protocol.OpFreezePosition:
		c.handleFreezePosition(msg.Payload)

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
		// Auto-enable god mode for admins
		if c.server.AuthMgr != nil && c.userSub != "" {
			user := c.server.AuthMgr.UserStore.Get(c.userSub)
			if user != nil && user.IsAdmin {
				c.godMode = true
			}
		}
		c.mu.Unlock()
		// Send full world sync for initial spectator view
		allCells := c.engine.AllCells()
		syncBuilder := protocol.NewWorldUpdateBuilder(c.shuffle, nil)
		maxID := game.MaxCellID()
		newSet := make([]bool, maxID+256)
		newIDs := make([]uint32, 0, len(allCells))
		for _, cell := range allCells {
			syncBuilder.AddCell(cell)
			if int(cell.ID) < len(newSet) {
				newSet[cell.ID] = true
			}
			newIDs = append(newIDs, cell.ID)
		}
		c.sendMsg(syncBuilder.Finish(nil))
		c.mu.Lock()
		c.knownSet = newSet
		c.knownIDs = newIDs
		c.mu.Unlock()

	case protocol.OpStatUpdate:
		// Keepalive acknowledged, reset deadline
		c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	case protocol.OpBoostCheck:
		// Spectator commands: 0x01 = toggle follow
		if len(msg.Payload) >= 1 {
			cmd := msg.Payload[0]
			c.mu.Lock()
			switch cmd {
			case 0x01:
				c.spectatorFollowing = !c.spectatorFollowing
				if c.spectatorFollowing {
					c.spectateTarget = 0 // reset to auto-follow top player
				}
			}
			c.mu.Unlock()
		}
	}
}

// activePlayer returns the player for the currently active multibox slot.
func (c *Client) activePlayer() *game.Player {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.multiEnabled && c.multiSlot == 1 {
		return c.multiPlayer
	}
	return c.player
}

// handleMultiboxToggle enables or disables multibox for this client.
func (c *Client) handleMultiboxToggle() {
	c.mu.Lock()
	wasEnabled := c.multiEnabled

	if wasEnabled {
		// Disable multibox: remove multi player from engine
		c.multiEnabled = false
		c.multiSlot = 0
		mp := c.multiPlayer
		c.multiPlayer = nil
		c.multiAlive = false
		c.multiRespawn = 0
		c.knownMultiCells = nil
		c.mu.Unlock()

		if mp != nil {
			c.engine.QueueRemovePlayer(mp.ID)
		}

		c.sendMsg(protocol.BuildMultiboxState(c.shuffle, false, 0, false))
		log.Printf("Multibox disabled for player %q", c.player.Name)
	} else {
		// Enable multibox: create second player near the primary
		if c.player == nil || !c.player.Alive {
			c.mu.Unlock()
			return
		}
		name := c.player.Name
		skin := c.player.Skin
		effect := c.player.Effect
		cx, cy := c.player.Center()

		c.multiEnabled = true
		c.multiSlot = 0
		c.knownMultiCells = make(map[uint32]bool, 16)
		c.mu.Unlock()

		// Create the multi player with same name/skin/effect, spawn near primary
		mp := game.NewPlayer(name, skin, effect)
		mp.Color = c.player.Color
		mp.IsSubscriber = c.player.IsSubscriber
		mp.Clan = c.player.Clan
		mp.Conn = c
		c.engine.AddPlayer(mp)
		c.engine.SpawnPlayerNear(mp, cx, cy, 500)

		c.mu.Lock()
		c.multiPlayer = mp
		c.multiAlive = true
		c.mu.Unlock()

		// Send AddMultiCell for the newly spawned multi cells
		for _, cell := range mp.Cells {
			c.sendMsg(protocol.BuildAddMultiCell(c.shuffle, cell.ID))
			c.mu.Lock()
			c.knownMultiCells[cell.ID] = true
			c.mu.Unlock()
		}

		c.sendMsg(protocol.BuildMultiboxState(c.shuffle, true, 0, true))
		log.Printf("Multibox enabled for player %q (multi ID %d)", name, mp.ID)
	}
}

// handleMultiboxSwitch switches the active multibox slot (Tab key).
// No viewport change — both players share a viewport. Just flips which
// player receives mouse/split/eject commands.
func (c *Client) handleMultiboxSwitch() {
	c.mu.Lock()
	if !c.multiEnabled {
		c.mu.Unlock()
		return
	}

	if c.multiSlot == 0 {
		c.multiSlot = 1
	} else {
		c.multiSlot = 0
	}
	newSlot := c.multiSlot
	multiAlive := c.multiAlive
	c.mu.Unlock()

	c.sendMsg(protocol.BuildMultiboxState(c.shuffle, true, newSlot, multiAlive))
}

// handleDirectionLock locks or unlocks the active player's movement direction.
// When locked, the player's cells keep moving in the direction they were heading
// at the moment of lock; mouse input is used only for eject/split aiming.
func (c *Client) handleDirectionLock(payload []byte) {
	if len(payload) < 1 {
		return
	}
	lock := payload[0] == 1
	p := c.activePlayer()
	if p == nil || !p.Alive {
		return
	}

	p.SetDirectionLock(lock)
}

// handleFreezePosition freezes or unfreezes the active player's cell positions.
// When frozen, cells stop all movement — they stay exactly where they are.
func (c *Client) handleFreezePosition(payload []byte) {
	if len(payload) < 1 {
		return
	}
	freeze := payload[0] == 1
	p := c.activePlayer()
	if p == nil || !p.Alive {
		return
	}
	p.SetFrozen(freeze)
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
	defer func() {
		ticker.Stop()
		// Close the connection so handleConnection's ReadMessage unblocks immediately.
		// Without this, ghost cells linger until the read deadline (30-60s) fires.
		c.conn.Close()
	}()

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

// CheckInactiveClients scans all connected clients and force-disconnects any
// that have not sent a message (or pong) in the last 10 seconds. This prevents
// ghost cells from lingering when a connection goes silently dead.
func (s *Server) CheckInactiveClients() {
	cutoff := time.Now().Add(-10 * time.Second).UnixNano()

	s.mu.RLock()
	var stale []*Client
	for c := range s.clients {
		last := sync_atomic.LoadInt64(&c.lastActivity)
		if last > 0 && last < cutoff {
			stale = append(stale, c)
		}
	}
	s.mu.RUnlock()

	for _, c := range stale {
		name := "unknown"
		if c.player != nil {
			name = c.player.Name
		}
		log.Printf("Inactivity timeout (10s): disconnecting client %q from %s", name, c.remoteIP)
		c.conn.Close()
	}
}

// Broadcast sends a viewport-culled world update to each connected client.
// Uses flat []bool arrays instead of maps for O(1) lookups with no hash overhead.
func (s *Server) Broadcast(updated []*game.Cell, eaten []game.EatEvent, removed []uint32, tickNum uint64) {
	grid := s.Engine.Grid

	// Build flat array of updated cell IDs for O(1) lookup (replaces map[uint32]bool).
	maxID := game.MaxCellID()
	arrLen := int(maxID) + 1
	updatedArr := make([]bool, arrLen)
	for _, c := range updated {
		if int(c.ID) < arrLen {
			updatedArr[c.ID] = true
		}
	}

	// Snapshot client list under brief lock, then release
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		if c.shuffle != nil {
			clients = append(clients, c)
		}
	}
	s.mu.RUnlock()

	if len(clients) == 0 {
		return
	}

	cfg := s.Engine.Cfg

	// Process each client concurrently, but limit parallelism to avoid
	// cache thrashing on low-core Docker containers.
	maxWorkers := runtime.NumCPU()
	if maxWorkers < 2 {
		maxWorkers = 2
	}
	if maxWorkers > len(clients) {
		maxWorkers = len(clients)
	}

	var wg sync.WaitGroup
	work := make(chan *Client, len(clients))
	for _, c := range clients {
		work <- c
	}
	close(work)

	wg.Add(maxWorkers)
	for i := 0; i < maxWorkers; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// Client disconnected mid-broadcast (send on closed channel) — safe to ignore
				}
			}()
			// Pre-allocate worker-local buffers to avoid per-client GC pressure.
			seen := make([]bool, arrLen)
			newIDs := make([]uint32, 0, 512)
			var visBuf []*game.Cell
			for c := range work {
				newIDs, visBuf = s.broadcastToClient(c, cfg, updatedArr, eaten, removed, tickNum, grid, arrLen, seen, newIDs, visBuf)
				// Clear only the bits that were set (faster than zeroing entire array).
				for _, id := range newIDs {
					if int(id) < len(seen) {
						seen[id] = false
					}
				}
			}
		}()
	}
	wg.Wait()
}

// broadcastToClient sends the world update to a single client.
// Called concurrently from Broadcast — all shared data (updatedArr, eaten, removed, grid) is read-only.
// Uses flat []bool arrays for O(1) lookups (no hash maps in the hot path).
// Eliminates CellInViewport by using grid query as the "seen" set for viewport exit detection.
// seen, newIDs, and visBuf are worker-local buffers passed in to avoid per-client allocations.
// Returns newIDs and visBuf so the caller can reuse them.
func (s *Server) broadcastToClient(client *Client, cfg game.Config, updatedArr []bool, eaten []game.EatEvent, removed []uint32, tickNum uint64, grid *game.SpatialGrid, arrLen int, seen []bool, newIDs []uint32, visBuf []*game.Cell) ([]uint32, []*game.Cell) {
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
	var camZoom float32
	if isSpectating {
		if godMode {
			// Admin god mode: see entire map, centered at (0,0)
			vp = game.Viewport{Left: -cfg.MapWidth, Top: -cfg.MapHeight, Right: cfg.MapWidth, Bottom: cfg.MapHeight}
			camX = 0
			camY = 0
			// Zoom to fit entire map in a 1920x1080 reference
			camZoom = float32(960.0 / cfg.MapWidth)
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
				// Compute zoom matching the followed player (OgarII formula)
				var totalSize float64
				for _, c := range targetPlayer.Cells {
					totalSize += c.Size
				}
				if totalSize < 1 {
					totalSize = 1
				}
				ratio := 64.0 / totalSize
				if ratio > 1.0 {
					ratio = 1.0
				}
				camZoom = float32(math.Pow(ratio, 0.4))
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
			camZoom = 0.4 // OgarII playerRoamViewScale
		}
	} else {
		// Determine active player (primary or multi based on slot)
		activeP := client.player
		client.mu.Lock()
		multiEnabled := client.multiEnabled
		var multiP *game.Player
		if multiEnabled && client.multiPlayer != nil {
			multiP = client.multiPlayer
			if client.multiSlot == 1 {
				activeP = multiP
			}
		}
		client.mu.Unlock()

		if multiEnabled && multiP != nil {
			// Shared viewport: centered on active, sized by combined mass
			vp = game.ViewportForMultibox(activeP, multiP, cfg.MapWidth, cfg.MapHeight)
		} else {
			vp = game.ViewportForPlayer(activeP, cfg.MapWidth, cfg.MapHeight)
		}
	}

	// Determine active player and multibox state (needed for own-cell inclusion)
	client.mu.Lock()
	activeP := client.player
	multiEnabled := client.multiEnabled
	var multiP *game.Player
	if multiEnabled && client.multiPlayer != nil {
		multiP = client.multiPlayer
		if client.multiSlot == 1 {
			activeP = client.multiPlayer
		}
	}

	// Ensure knownSet is large enough for current cell IDs
	if len(client.knownSet) < arrLen {
		grown := make([]bool, arrLen+256)
		copy(grown, client.knownSet)
		client.knownSet = grown
	}
	knownSet := client.knownSet

	builder := protocol.NewWorldUpdateBuilder(client.shuffle, eaten)

	// seen and newIDs are worker-local buffers (pre-allocated, zeroed by caller).
	// seen marks cells returned by grid query this tick for viewport exit detection.
	// Grow seen if needed (cell IDs can grow between ticks).
	if len(seen) < arrLen {
		seen = make([]bool, arrLen)
	}
	newIDs = newIDs[:0]

	// Grid query: find cells near the viewport.
	// Margin of 500 catches large cells whose center is outside but body overlaps.
	// We trust the grid + margin and skip CellInViewport (saves ~23% CPU).
	// Margin of 1500 (3× grid cell size) ensures cells enter/leave the server's
	// tracking well off-screen, so the client never sees pop-in/pop-out.
	// Client does its own precise viewport culling during rendering.
	visible := grid.QueryRect(visBuf[:0], vp.Left, vp.Top, vp.Right, vp.Bottom, 1500)
	for _, c := range visible {
		id := c.ID
		if int(id) >= arrLen {
			continue
		}
		seen[id] = true
		newIDs = append(newIDs, id)
		if !knownSet[id] {
			builder.AddCell(c)
			knownSet[id] = true
		} else if updatedArr[id] {
			builder.AddCell(c)
		}
	}

	// Also include own cells that might be outside the grid query range
	if !isSpectating && client.player != nil && client.player.Alive {
		for _, c := range client.player.Cells {
			id := c.ID
			if int(id) >= arrLen {
				continue
			}
			if !seen[id] {
				seen[id] = true
				newIDs = append(newIDs, id)
			}
			if !knownSet[id] {
				builder.AddCell(c)
				knownSet[id] = true
			} else if updatedArr[id] {
				builder.AddCell(c)
			}
		}
		if multiEnabled && multiP != nil && multiP.Alive {
			for _, c := range multiP.Cells {
				id := c.ID
				if int(id) >= arrLen {
					continue
				}
				if !seen[id] {
					seen[id] = true
					newIDs = append(newIDs, id)
				}
				if !knownSet[id] {
					builder.AddCell(c)
					knownSet[id] = true
				} else if updatedArr[id] {
					builder.AddCell(c)
				}
			}
		}
	}

	// Build removal list:
	// 1. Cells explicitly removed/eaten server-side
	// 2. Cells the client knew about that left the viewport (not in this tick's "seen" set)
	var clientRemoved []uint32
	for _, id := range removed {
		if int(id) < arrLen && knownSet[id] {
			clientRemoved = append(clientRemoved, id)
			knownSet[id] = false
		}
		delete(client.knownMyCells, id)
		if client.knownMultiCells != nil {
			delete(client.knownMultiCells, id)
		}
	}

	// Viewport exit detection: iterate previous known IDs, remove those not "seen" this tick.
	// This replaces the old approach of calling CellInViewport on every known cell (~23% CPU savings).
	for _, id := range client.knownIDs {
		if int(id) >= arrLen || !knownSet[id] {
			continue // already removed above
		}
		if !seen[id] {
			// Cell is no longer in the grid query area (left viewport or died)
			clientRemoved = append(clientRemoved, id)
			knownSet[id] = false
			delete(client.knownMyCells, id)
			if client.knownMultiCells != nil {
				delete(client.knownMultiCells, id)
			}
		}
	}

	// Update knownIDs for next tick (flat slice of all currently known cells)
	client.knownIDs = newIDs

	client.mu.Unlock()

	msg := builder.Finish(clientRemoved)
	client.sendMsg(msg)

	// Send AddMyCell for primary player's cells
	if client.player != nil && client.player.Alive && !isSpectating {
		for _, cell := range client.player.Cells {
			if !client.knownMyCells[cell.ID] {
				client.sendMsg(protocol.BuildAddMyCell(client.shuffle, cell.ID))
				client.knownMyCells[cell.ID] = true
			}
		}
	}

	// Send AddMultiCell for multi player's cells
	if multiEnabled && multiP != nil && multiP.Alive && !isSpectating {
		client.mu.Lock()
		knownMulti := client.knownMultiCells
		client.mu.Unlock()
		if knownMulti != nil {
			for _, cell := range multiP.Cells {
				if !knownMulti[cell.ID] {
					client.sendMsg(protocol.BuildAddMultiCell(client.shuffle, cell.ID))
					client.mu.Lock()
					client.knownMultiCells[cell.ID] = true
					client.mu.Unlock()
				}
			}
		}
	}

	// Send camera update
	if isSpectating {
		cam := protocol.BuildCamera(client.shuffle, float32(camX), float32(camY), camZoom)
		client.sendMsg(cam)
	} else if activeP != nil && activeP.Alive {
		cx, cy := activeP.Center()
		cam := protocol.BuildCamera(client.shuffle, float32(cx), float32(cy), 0)
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

	// Check if primary player died
	client.mu.Lock()
	wasAlive := client.alive
	client.mu.Unlock()
	primaryDied := client.player != nil && wasAlive && !client.player.Alive
	if primaryDied {
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

		client.mu.Lock()
		client.alive = false
		client.knownMyCells = make(map[uint32]bool, 16)
		client.mu.Unlock()

		if multiEnabled && client.multiAlive {
			// Multi still alive — schedule primary auto-respawn, don't end game
			client.mu.Lock()
			client.primaryRespawn = time.Now().Unix() + 3
			client.mu.Unlock()
		} else {
			// No multi alive — game over; cancel any pending respawn timers
			client.mu.Lock()
			client.primaryRespawn = 0
			client.multiRespawn = 0
			client.mu.Unlock()
			client.sendMsg(protocol.BuildClearMine(client.shuffle))
		}
	}

	// Multibox: check if multi player died
	if multiEnabled && multiP != nil {
		client.mu.Lock()
		multiWasAlive := client.multiAlive
		client.mu.Unlock()

		if multiWasAlive && !multiP.Alive {
			client.mu.Lock()
			client.multiAlive = false
			client.knownMultiCells = make(map[uint32]bool, 16)
			client.mu.Unlock()

			if client.alive {
				// Primary still alive — schedule multi auto-respawn
				client.mu.Lock()
				client.multiRespawn = time.Now().Unix() + 3
				client.mu.Unlock()
			} else {
				// Both dead now — game over; cancel any pending respawn timers
				client.mu.Lock()
				client.primaryRespawn = 0
				client.multiRespawn = 0
				client.mu.Unlock()
				client.sendMsg(protocol.BuildClearMine(client.shuffle))
			}

			client.sendMsg(protocol.BuildMultiboxState(client.shuffle, true, client.multiSlot, false))
		}

		// Auto-respawn multi player near primary
		client.mu.Lock()
		respawnTime := client.multiRespawn
		client.mu.Unlock()

		if respawnTime > 0 && time.Now().Unix() >= respawnTime {
			if client.player != nil && client.player.Alive {
				cx, cy := client.player.Center()
				s.Engine.SpawnPlayerNear(multiP, cx, cy, 500)
			} else {
				s.Engine.SpawnPlayer(multiP)
			}

			client.mu.Lock()
			client.multiAlive = true
			client.multiRespawn = 0
			client.mu.Unlock()

			for _, cell := range multiP.Cells {
				client.sendMsg(protocol.BuildAddMultiCell(client.shuffle, cell.ID))
				client.mu.Lock()
				client.knownMultiCells[cell.ID] = true
				client.mu.Unlock()
			}

			client.sendMsg(protocol.BuildMultiboxState(client.shuffle, true, client.multiSlot, true))
		}
	}

	// Auto-respawn primary player near multi
	if multiEnabled {
		client.mu.Lock()
		primaryRespawnTime := client.primaryRespawn
		client.mu.Unlock()

		if primaryRespawnTime > 0 && time.Now().Unix() >= primaryRespawnTime {
			if multiP != nil && multiP.Alive {
				mx, my := multiP.Center()
				s.Engine.SpawnPlayerNear(client.player, mx, my, 500)
			} else {
				s.Engine.SpawnPlayer(client.player)
			}

			client.mu.Lock()
			client.alive = true
			client.primaryRespawn = 0
			client.mu.Unlock()

			newMyCells := make(map[uint32]bool, len(client.player.Cells))
			for _, cell := range client.player.Cells {
				client.sendMsg(protocol.BuildAddMyCell(client.shuffle, cell.ID))
				newMyCells[cell.ID] = true
			}
			client.mu.Lock()
			client.knownMyCells = newMyCells
			client.mu.Unlock()
		}
	}
	return newIDs, visible
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

	// Forward to external webhook if configured
	if s.ChatBridge != nil {
		s.ChatBridge.SendOutgoing(sender, text)
	}
}

// BroadcastClanChat sends a clan chat message only to clients that are members of the given clan.
// This is private clan chat — NOT forwarded to the IRC/Discord bridge.
func (s *Server) BroadcastClanChat(clanID, senderName, text string) {
	if s.ClanStore == nil {
		return
	}
	clan := s.ClanStore.GetClan(clanID)
	if clan == nil {
		return
	}

	// Build set of clan member subs for O(1) lookup
	memberSubs := make(map[string]bool, len(clan.Members))
	for _, m := range clan.Members {
		memberSubs[m.Sub] = true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for client := range s.clients {
		if client.shuffle == nil || !memberSubs[client.userSub] {
			continue
		}
		// Use a neutral color (clan-themed) for clan chat
		msg := protocol.BuildClanChat(client.shuffle, 100, 200, 255, senderName, text)
		client.sendMsg(msg)
	}
}

// BroadcastSystemChat sends a system chat message to all connected clients.
func (s *Server) BroadcastSystemChat(text string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for client := range s.clients {
		if client.shuffle == nil {
			continue
		}
		// System messages in yellow
		msg := protocol.BuildChat(client.shuffle, 0, 255, 200, 50, "[Server]", text)
		client.sendMsg(msg)
	}
}

// BroadcastClanPositions sends clan member positions to all online clan members.
// Called periodically from the tick loop for clans with online members.
func (s *Server) BroadcastClanPositions() {
	if s.ClanStore == nil {
		return
	}

	s.mu.RLock()
	// Build a map: userSub → (player, client)
	type clientInfo struct {
		client *Client
		player *game.Player
	}
	onlineUsers := make(map[string]clientInfo, len(s.clients))
	for c := range s.clients {
		if c.shuffle == nil || c.userSub == "" {
			continue
		}
		if c.player != nil && c.player.Alive {
			onlineUsers[c.userSub] = clientInfo{client: c, player: c.player}
		}
	}
	s.mu.RUnlock()

	if len(onlineUsers) == 0 {
		return
	}

	// For each clan that has online members, build and send positions
	s.ClanStore.mu.RLock()
	defer s.ClanStore.mu.RUnlock()

	for _, clan := range s.ClanStore.clans {
		// Collect online clan members
		var onlineMembers []struct {
			sub    string
			player *game.Player
			ci     clientInfo
		}
		for _, m := range clan.Members {
			if ci, ok := onlineUsers[m.Sub]; ok {
				onlineMembers = append(onlineMembers, struct {
					sub    string
					player *game.Player
					ci     clientInfo
				}{sub: m.Sub, player: ci.player, ci: ci})
			}
		}

		if len(onlineMembers) < 2 {
			continue // need at least 2 online to share positions
		}

		// Build position data for all online members
		var positions []protocol.ClanMemberPos
		for _, om := range onlineMembers {
			cx, cy := om.player.Center()
			var totalSize float64
			for _, cell := range om.player.Cells {
				totalSize += cell.Size
			}
			positions = append(positions, protocol.ClanMemberPos{
				X:    cx,
				Y:    cy,
				Size: totalSize,
				Skin: om.player.Skin,
				Name: om.player.Name,
			})
		}

		// Send to each online clan member
		for _, om := range onlineMembers {
			msg := protocol.BuildClanPositions(om.ci.client.shuffle, positions)
			om.ci.client.sendMsg(msg)
		}
	}
}

// BroadcastBattleRoyale sends the BR zone state to all connected clients.
func (s *Server) BroadcastBattleRoyale() {
	if s.BattleRoyale == nil {
		return
	}

	info := s.BattleRoyale.GetInfo()

	s.mu.RLock()
	defer s.mu.RUnlock()

	for client := range s.clients {
		if client.shuffle == nil {
			continue
		}
		msg := protocol.BuildBattleRoyale(client.shuffle,
			byte(info.State), info.PlayersAlive, info.Countdown,
			info.TimeRemaining, info.ZoneCX, info.ZoneCY, info.ZoneRadius,
			info.WinnerName)
		client.sendMsg(msg)
	}
}

// TickBattleRoyale runs the BR tick and broadcasts updates.
func (s *Server) TickBattleRoyale() {
	if s.BattleRoyale == nil {
		return
	}

	// Get players from the engine (read lock for the map reference)
	s.Engine.RLock()
	players := s.Engine.Players()
	s.Engine.RUnlock()

	// BR tick returns a list of player IDs to kill this tick
	kills := s.BattleRoyale.Tick(players)

	// Remove killed players' cells from the engine (acquires write lock)
	s.Engine.KillPlayersForBR(kills)
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

// freeEffects is the set of border effects available to all players.
var freeEffects = map[string]bool{
	"neon":      true,
	"prismatic": true,
	"starfield": true,
	"lightning": true,
}

// premiumEffects is the set of premium effects that require unlocking.
var premiumEffects = map[string]bool{
	"sakura":      true,
	"frost":       true,
	"shadow_aura": true,
	"flame":       true,
	"glitch":      true,
	"blackhole":   true,
	"trail":       true,
}

// GetPremiumEffectNames returns the list of premium effect IDs.
func GetPremiumEffectNames() []string {
	names := make([]string, 0, len(premiumEffects))
	for k := range premiumEffects {
		names = append(names, k)
	}
	return names
}

// validateEffect checks if this client is allowed to use the requested effect.
// Free effects are available to all. Premium effects require unlocking.
func (c *Client) validateEffect(effect string) string {
	if effect == "" {
		return ""
	}

	// Free effects — anyone can use
	if freeEffects[effect] {
		return effect
	}

	// Premium effects — require unlocking
	if premiumEffects[effect] {
		// Guest (no user sub) — cannot use premium effects
		if c.userSub == "" {
			return ""
		}

		if c.server.AuthMgr == nil {
			return effect // no auth system, allow
		}
		user := c.server.AuthMgr.UserStore.Get(c.userSub)
		if user == nil {
			return ""
		}

		// Admin can use any effect
		if user.IsAdmin {
			return effect
		}

		// Check if the user has unlocked this effect
		if stringInSlice(user.UnlockedEffects, effect) {
			return effect
		}
		return "" // not unlocked
	}

	return "" // unknown effect
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

// EffectAccessEntry is one effect with access info for a specific user.
type EffectAccessEntry struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Category    string `json:"category"`   // "free" or "premium"
	Accessible  bool   `json:"accessible"` // can the user equip this effect?
	Reason      string `json:"reason,omitempty"`
	Tokens      int    `json:"tokens,omitempty"`
	TokensNeed  int    `json:"tokensNeed,omitempty"`
}

// effectsManifest is the static list of all effects for the access endpoint.
var effectsManifest = []struct {
	ID          string
	Label       string
	Description string
	Category    string
}{
	{"neon", "Neon Pulse", "Pulsing neon glow around your cell", "free"},
	{"prismatic", "Prismatic", "Shifting rainbow border", "free"},
	{"starfield", "Starfield", "Orbiting stars around your cell", "free"},
	{"lightning", "Lightning", "Crackling electric arcs", "free"},
	{"sakura", "Sakura", "Cherry blossom petals drifting around your cell", "premium"},
	{"frost", "Frost", "Ice crystals and frosty mist surrounding your cell", "premium"},
	{"shadow_aura", "Shadow Aura", "Dark smoke tendrils — menacing dark energy", "premium"},
	{"flame", "Flame", "Rising fire particles around your cell", "premium"},
	{"glitch", "Glitch", "Digital distortion and RGB shift effect", "premium"},
	{"blackhole", "Black Hole", "Dark gravity well with warped accretion rings", "premium"},
	{"trail", "Trail", "Smooth ribbon trail following your cell", "premium"},
}

// HandleEffectsAccess returns all effects with per-user access info.
// GET /api/effects/access?session=TOKEN
func (s *Server) HandleEffectsAccess(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
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

	result := make([]EffectAccessEntry, 0, len(effectsManifest))
	for _, ef := range effectsManifest {
		entry := EffectAccessEntry{
			ID:          ef.ID,
			Label:       ef.Label,
			Description: ef.Description,
			Category:    ef.Category,
		}

		if ef.Category == "free" {
			entry.Accessible = true
		} else if user == nil {
			entry.Accessible = false
			entry.Reason = "Sign in to unlock"
		} else if isAdmin {
			entry.Accessible = true
		} else {
			tokens := 0
			if user.EffectTokens != nil {
				tokens = user.EffectTokens[ef.ID]
			}
			entry.Tokens = tokens
			entry.TokensNeed = TokensPerEffectUnlock
			if stringInSlice(user.UnlockedEffects, ef.ID) {
				entry.Accessible = true
			} else {
				entry.Accessible = false
				entry.Reason = fmt.Sprintf("%d/%d tokens", tokens, TokensPerEffectUnlock)
			}
		}

		result = append(result, entry)
	}

	resp := map[string]interface{}{
		"effects": result,
	}
	if user != nil {
		resp["level"] = LevelFromPoints(user.Points)
		resp["pendingEffectTokens"] = user.PendingEffectTokens
	}

	json.NewEncoder(w).Encode(resp)
}

// HandleTopUsers returns the top users by all-time points (excluding bot names).
// GET /api/top-users?limit=20
func (s *Server) HandleTopUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	limit := 20
	if lq := r.URL.Query().Get("limit"); lq != "" {
		var n int
		if _, err := fmt.Sscanf(lq, "%d", &n); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}

	if s.AuthMgr == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"topUsers": []interface{}{}})
		return
	}
	allUsers := s.AuthMgr.UserStore.GetAll()

	// Filter out users with no points and sort by points descending
	type topEntry struct {
		Name     string `json:"name"`
		Points   int64  `json:"points"`
		Level    int    `json:"level"`
		TopScore int64  `json:"topScore"`
	}

	entries := make([]topEntry, 0, len(allUsers))
	for _, u := range allUsers {
		if u.Points <= 0 {
			continue
		}
		// Skip banned users
		if u.IsBanned() {
			continue
		}
		name := u.Name
		if name == "" {
			name = "unnamed"
		}
		entries = append(entries, topEntry{
			Name:     name,
			Points:   u.Points,
			Level:    LevelFromPoints(u.Points),
			TopScore: u.TopScore,
		})
	}

	// Sort by points descending
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Points > entries[i].Points {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	if len(entries) > limit {
		entries = entries[:limit]
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"topUsers": entries,
	})
}
