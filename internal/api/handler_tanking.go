package api

import (
	"encoding/json"
	"log"
	"time"

	"github.com/h4ks-com/h4kmally-server/internal/game"
	"github.com/h4ks-com/h4kmally-server/internal/protocol"
)

// handleTankQueue handles OpTankQueue: the client wants to form or join a tank.
func (c *Client) handleTankQueue(payload []byte) {
	var p protocol.TankQueuePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("Bad tank queue payload: %v", err)
		c.sendTankError("Invalid request")
		return
	}

	// Validate size: 2, 3, or 4
	if p.Size < 2 || p.Size > 4 {
		c.sendTankError("Tank size must be 2, 3, or 4")
		return
	}

	// Don't allow tanking if already in a tank or multibox
	c.mu.Lock()
	if c.inTank {
		c.mu.Unlock()
		c.sendTankError("Already in a tank session")
		return
	}
	if c.multiEnabled {
		c.mu.Unlock()
		c.sendTankError("Disable multibox before tanking")
		return
	}
	c.mu.Unlock()

	if c.server.TankMgr == nil {
		c.sendTankError("Tanking not available")
		return
	}

	name := p.Name
	if name == "" {
		name = "Tank Player"
	}

	member := &game.TankMember{
		ConnID:    c,
		Name:      name,
		UserSub:   c.userSub,
		Connected: true,
		Skin:      p.Skin,
		Effect:    p.Effect,
	}

	var session *game.TankSession
	if p.Private {
		session = c.server.TankMgr.CreatePrivate(p.Size, member)
	} else {
		session = c.server.TankMgr.QueuePublic(p.Size, member)
	}

	if session == nil {
		c.sendTankError("Failed to create tank session")
		return
	}

	c.mu.Lock()
	c.inTank = true
	c.tankSession = session
	c.mu.Unlock()

	log.Printf("Player %q joined tank session %s (size=%d, private=%v, state=%s)",
		name, session.Code, session.DesiredSize, session.Private, session.State.String())

	// Broadcast updated state to all members
	c.server.broadcastTankState(session)

	// If voting started (session filled up), kick off the vote timer
	if session.State == game.TankVoting {
		go c.server.runTankVoteTimer(session)
	}
}

// handleTankJoin handles OpTankJoin: join a private tank session by code.
func (c *Client) handleTankJoin(payload []byte) {
	var p protocol.TankJoinPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("Bad tank join payload: %v", err)
		c.sendTankError("Invalid request")
		return
	}

	if p.Code == "" {
		c.sendTankError("No join code provided")
		return
	}

	c.mu.Lock()
	if c.inTank {
		c.mu.Unlock()
		c.sendTankError("Already in a tank session")
		return
	}
	if c.multiEnabled {
		c.mu.Unlock()
		c.sendTankError("Disable multibox before tanking")
		return
	}
	c.mu.Unlock()

	if c.server.TankMgr == nil {
		c.sendTankError("Tanking not available")
		return
	}

	name := p.Name
	if name == "" {
		name = "Tank Player"
	}

	member := &game.TankMember{
		ConnID:    c,
		Name:      name,
		UserSub:   c.userSub,
		Connected: true,
		Skin:      p.Skin,
		Effect:    p.Effect,
	}

	session := c.server.TankMgr.JoinPrivate(p.Code, member)
	if session == nil {
		c.sendTankError("Session not found or full")
		return
	}

	c.mu.Lock()
	c.inTank = true
	c.tankSession = session
	c.mu.Unlock()

	log.Printf("Player %q joined private tank session %s", name, session.Code)

	// Broadcast updated state to all members
	c.server.broadcastTankState(session)

	// If voting started, kick off the vote timer
	if session.State == game.TankVoting {
		go c.server.runTankVoteTimer(session)
	}
}

// handleTankCancel handles OpTankCancel: leave the tank session.
func (c *Client) handleTankCancel() {
	c.mu.Lock()
	inTank := c.inTank
	session := c.tankSession
	c.inTank = false
	c.tankSession = nil
	c.mu.Unlock()

	if !inTank || session == nil {
		return
	}

	if c.server.TankMgr != nil {
		c.server.TankMgr.RemoveMemberFromSession(c, session.Code)
	}

	log.Printf("Client left tank session %s", session.Code)

	// Broadcast updated state to remaining members
	c.server.broadcastTankState(session)
}

// handleTankVote handles OpTankVote: cast skin/effect vote during voting phase.
func (c *Client) handleTankVote(payload []byte) {
	var v protocol.TankVotePayload
	if err := json.Unmarshal(payload, &v); err != nil {
		log.Printf("Bad tank vote payload: %v", err)
		return
	}

	c.mu.Lock()
	session := c.tankSession
	c.mu.Unlock()

	if session == nil {
		return
	}

	session.Vote(c, v.Skin, v.Effect)

	// Broadcast updated state to all members
	c.server.broadcastTankState(session)

	// If all voted, start the game immediately (don't wait for timer)
	if session.AllVoted() {
		c.server.startTankGame(session)
	}
}

// handleTankMemberDisconnect handles a tank member disconnecting.
func (s *Server) handleTankMemberDisconnect(c *Client, session *game.TankSession) {
	session.RemoveMember(c)

	connected := session.ConnectedCount()
	if connected == 0 {
		// All members gone — remove shared player and clean up
		if session.Player != nil {
			s.Engine.QueueRemovePlayer(session.Player.ID)
		}
		if s.TankMgr != nil {
			s.TankMgr.RemoveSession(session.Code)
		}
		session.End()
		log.Printf("Tank session %s ended (all members disconnected)", session.Code)
		return
	}

	// Update the shared player's TankMemberCount for decay floor adjustment
	if session.Player != nil {
		session.Player.TankMemberCount = connected
	}

	// Update all remaining members' cell names
	if session.Player != nil {
		session.Player.Name = session.CombinedName()
		// Update existing cells with new name
		for _, cell := range session.Player.Cells {
			cell.Name = session.Player.Name
		}
	}

	log.Printf("Tank member disconnected from session %s, %d remaining", session.Code, connected)

	// Broadcast updated state
	s.broadcastTankState(session)
}

// sendTankError sends a tank lobby error message to the client.
func (c *Client) sendTankError(msg string) {
	state := tankLobbyJSON{
		State: "error",
		Error: msg,
	}
	data, _ := json.Marshal(state)
	c.sendMsg(protocol.BuildTankLobby(c.shuffle, data))
}

// ── Tank lobby state broadcasting ──

type tankLobbyJSON struct {
	State       string           `json:"state"`
	Code        string           `json:"code,omitempty"`
	DesiredSize int              `json:"desiredSize,omitempty"`
	Members     []tankMemberJSON `json:"members,omitempty"`
	WaitTimer   int              `json:"waitTimer,omitempty"`
	VoteTimer   int              `json:"voteTimer,omitempty"`
	AllSkins    []string         `json:"allSkins,omitempty"`
	AllEffects  []string         `json:"allEffects,omitempty"`
	Error       string           `json:"error,omitempty"`
}

type tankMemberJSON struct {
	Name   string `json:"name"`
	IsHost bool   `json:"isHost,omitempty"`
	Voted  bool   `json:"voted,omitempty"`
	Skin   string `json:"skin,omitempty"`
	Effect string `json:"effect,omitempty"`
}

// broadcastTankState sends the current tank lobby state to all connected members.
func (s *Server) broadcastTankState(session *game.TankSession) {
	session.Lock()
	defer session.Unlock()

	state := tankLobbyJSON{
		State:       session.State.String(),
		Code:        session.Code,
		DesiredSize: session.DesiredSize,
	}

	switch session.State {
	case game.TankWaiting:
		state.WaitTimer = session.WaitSecondsRemainingLocked()
	case game.TankVoting:
		state.VoteTimer = session.VoteSecondsRemainingLocked()
	}

	// Collect member info
	skinSet := map[string]bool{}
	effectSet := map[string]bool{}
	for _, m := range session.Members {
		mj := tankMemberJSON{
			Name:   m.Name,
			IsHost: m.IsHost,
			Voted:  m.HasVoted,
			Skin:   m.Skin,
			Effect: m.Effect,
		}
		state.Members = append(state.Members, mj)
		if m.Skin != "" {
			skinSet[m.Skin] = true
		}
		if m.Effect != "" {
			effectSet[m.Effect] = true
		}
	}

	// Build skin/effect option lists (union of all members' proposals)
	for skin := range skinSet {
		state.AllSkins = append(state.AllSkins, skin)
	}
	for effect := range effectSet {
		state.AllEffects = append(state.AllEffects, effect)
	}

	data, _ := json.Marshal(state)

	// Send to all connected members
	for _, m := range session.Members {
		if !m.Connected {
			continue
		}
		if client, ok := m.ConnID.(*Client); ok && client.shuffle != nil {
			client.sendMsg(protocol.BuildTankLobby(client.shuffle, data))
		}
	}
}

// runTankVoteTimer waits for the 10-second voting period, then starts the game.
func (s *Server) runTankVoteTimer(session *game.TankSession) {
	// Wait up to 10 seconds, checking every second if all have voted
	for i := 0; i < 10; i++ {
		time.Sleep(1 * time.Second)
		if session.State != game.TankVoting {
			return // already started or cancelled
		}
		// Broadcast updated timer
		s.broadcastTankState(session)
		if session.AllVoted() {
			break
		}
	}
	// Start the game
	if session.State == game.TankVoting {
		s.startTankGame(session)
	}
}

// startTankGame transitions a tank session from voting to playing.
func (s *Server) startTankGame(session *game.TankSession) {
	session.Lock()
	if session.State != game.TankVoting {
		session.Unlock()
		return
	}
	session.Unlock()

	// Resolve votes
	skin, effect := session.ResolveVotes()

	// Validate skin/effect - use any member's client for validation
	// (the shared player isn't a specific user, so we just check that the skin exists)
	session.Lock()
	var validatorClient *Client
	for _, m := range session.Members {
		if m.Connected {
			if cl, ok := m.ConnID.(*Client); ok {
				validatorClient = cl
				break
			}
		}
	}
	session.Unlock()

	if validatorClient != nil {
		skin = validatorClient.validateSkinAccess(skin)
		effect = validatorClient.validateEffect(effect)
	}

	// Create the shared player
	session.TransitionToPlaying(skin, effect)
	player := session.Player
	if player == nil {
		return
	}

	// Copy color from host
	session.Lock()
	for _, m := range session.Members {
		if m.IsHost {
			if cl, ok := m.ConnID.(*Client); ok && cl.player != nil {
				player.Color = cl.player.Color
			}
			break
		}
	}
	memberCount := len(session.Members)
	session.Unlock()

	// Add to engine and spawn with scaled size
	s.Engine.AddPlayer(player)
	startSize := s.Engine.Cfg.StartSize * float64(memberCount)
	s.Engine.SpawnPlayerWithSize(player, startSize)

	// Load powerups from all members onto the shared player
	player.PowerupInventory = make(map[string]int)
	session.Lock()
	for _, m := range session.Members {
		if m.UserSub != "" && s.AuthMgr != nil {
			// Load this member's powerups and merge into tank player
			tempPlayer := &game.Player{PowerupInventory: make(map[string]int)}
			if s.AuthMgr.UserStore.LoadPowerups(m.UserSub, tempPlayer) {
				for k, v := range tempPlayer.PowerupInventory {
					player.PowerupInventory[k] += v
				}
			}
		}
	}
	session.Unlock()

	// Wire each member's client to the shared player
	session.Lock()
	for _, m := range session.Members {
		if !m.Connected {
			continue
		}
		client, ok := m.ConnID.(*Client)
		if !ok || client.shuffle == nil {
			continue
		}

		// Set the shared player
		client.mu.Lock()
		client.player = player
		client.alive = true
		client.spectating = false

		// Full world sync
		allCells := s.Engine.AllCells()
		syncBuilder := protocol.NewWorldUpdateBuilder(client.shuffle, nil)
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

		client.knownSet = newSet
		client.knownIDs = newIDs
		client.knownMyCells = make(map[uint32]bool, len(player.Cells))
		client.mu.Unlock()

		// Send spawn result
		client.sendMsg(protocol.BuildSpawnResult(client.shuffle, true))

		// Send world sync
		client.sendMsg(syncBuilder.Finish(nil))

		// Send AddMyCell for each tank cell
		for _, cell := range player.Cells {
			client.sendMsg(protocol.BuildAddMyCell(client.shuffle, cell.ID))
			client.mu.Lock()
			client.knownMyCells[cell.ID] = true
			client.mu.Unlock()
		}

		// Send powerup state
		client.sendMsg(protocol.BuildPowerupState(client.shuffle, player.PowerupInventory))
	}
	session.Unlock()

	log.Printf("Tank session %s started with %d members, size %.0f",
		session.Code, memberCount, startSize)

	// Broadcast final playing state
	s.broadcastTankState(session)

	// Start cursor broadcasting goroutine
	go s.runTankCursorBroadcast(session)
}

// runTankCursorBroadcast periodically sends cursor positions of all tank members to each other.
func (s *Server) runTankCursorBroadcast(session *game.TankSession) {
	ticker := time.NewTicker(200 * time.Millisecond) // ~5Hz
	defer ticker.Stop()

	for range ticker.C {
		if session.State != game.TankPlaying {
			return
		}
		if session.Player == nil || !session.Player.Alive {
			return
		}

		session.Lock()
		// Build cursor entries for all connected members
		cursors := make([]protocol.TankCursorEntry, 0, len(session.Members))
		for _, m := range session.Members {
			if !m.Connected {
				continue
			}
			cursors = append(cursors, protocol.TankCursorEntry{
				Name: m.Name,
				X:    int16(m.MouseX),
				Y:    int16(m.MouseY),
			})
		}

		// Send to each member (all cursors including their own)
		for _, m := range session.Members {
			if !m.Connected {
				continue
			}
			if client, ok := m.ConnID.(*Client); ok && client.shuffle != nil {
				client.sendMsg(protocol.BuildTankCursors(client.shuffle, cursors))
			}
		}
		session.Unlock()
	}
}

// TickTankSessions handles per-tick tank maintenance:
// cursor updates, death detection, session cleanup.
// Called from the main broadcast loop.
func (s *Server) TickTankSessions() {
	if s.TankMgr == nil {
		return
	}

	// Clean up expired waiting sessions
	expired := s.TankMgr.CleanupExpired()
	for _, session := range expired {
		session.Lock()
		for _, m := range session.Members {
			if m.Connected {
				if client, ok := m.ConnID.(*Client); ok {
					client.mu.Lock()
					client.inTank = false
					client.tankSession = nil
					client.mu.Unlock()
					client.sendTankError("Tank session expired (not enough players)")
				}
			}
		}
		session.Unlock()
	}

	// Check for tank player deaths
	s.mu.RLock()
	clients := make([]*Client, 0)
	for c := range s.clients {
		c.mu.Lock()
		if c.inTank && c.tankSession != nil && c.tankSession.State == game.TankPlaying {
			clients = append(clients, c)
		}
		c.mu.Unlock()
	}
	s.mu.RUnlock()

	// Group by session to avoid duplicate handling
	handled := map[string]bool{}
	for _, c := range clients {
		c.mu.Lock()
		ts := c.tankSession
		c.mu.Unlock()
		if ts == nil || handled[ts.Code] {
			continue
		}
		handled[ts.Code] = true

		if ts.Player != nil && !ts.Player.Alive {
			// Tank player died — end the session
			s.endTankSession(ts)
		}
	}
}

// endTankSession cleans up a tank session when the tank player dies.
func (s *Server) endTankSession(session *game.TankSession) {
	session.End()

	// Broadcast ended state so clients clear their tank UI
	s.broadcastTankState(session)

	session.Lock()
	for _, m := range session.Members {
		if !m.Connected {
			continue
		}
		if client, ok := m.ConnID.(*Client); ok {
			client.mu.Lock()
			client.inTank = false
			client.tankSession = nil
			client.alive = false
			client.mu.Unlock()

			// Send ClearMine to return to lobby
			if client.shuffle != nil {
				client.sendMsg(protocol.BuildClearMine(client.shuffle))
			}

			// Record game stats for each member
			if m.UserSub != "" && s.AuthMgr != nil {
				score := int64(session.Player.Score)
				s.AuthMgr.UserStore.UpdatePoints(m.UserSub, score, score)
				s.AuthMgr.UserStore.RecordGame(m.UserSub)
			}
		}
	}
	session.Unlock()

	if s.TankMgr != nil {
		s.TankMgr.RemoveSession(session.Code)
	}

	log.Printf("Tank session %s ended (player died)", session.Code)
}

// broadcastTankAddMyCell sends AddMyCell for new tank cells to all members.
// Called from the broadcast loop when the tank player gets new cells (e.g., split).
func (s *Server) broadcastTankAddMyCell(session *game.TankSession) {
	if session == nil || session.Player == nil {
		return
	}

	session.Lock()
	defer session.Unlock()

	for _, m := range session.Members {
		if !m.Connected {
			continue
		}
		client, ok := m.ConnID.(*Client)
		if !ok || client.shuffle == nil {
			continue
		}

		for _, cell := range session.Player.Cells {
			client.mu.Lock()
			known := client.knownMyCells[cell.ID]
			client.mu.Unlock()
			if !known {
				client.sendMsg(protocol.BuildAddMyCell(client.shuffle, cell.ID))
				client.mu.Lock()
				client.knownMyCells[cell.ID] = true
				client.mu.Unlock()
			}
		}
	}
}
