package game

import (
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// TankState represents the lifecycle of a tank session.
type TankState int

const (
	TankWaiting TankState = iota // waiting for players to join
	TankVoting                   // all joined, voting on skin/effect
	TankPlaying                  // game in progress
	TankEnded                    // session finished
)

func (s TankState) String() string {
	switch s {
	case TankWaiting:
		return "waiting"
	case TankVoting:
		return "voting"
	case TankPlaying:
		return "playing"
	case TankEnded:
		return "ended"
	default:
		return "unknown"
	}
}

// TankMember represents one participant in a tank session.
type TankMember struct {
	// ConnID is an opaque identifier for the connection (set externally by api layer).
	ConnID interface{}

	Name    string
	UserSub string

	// Mouse position (this member's individual cursor)
	MouseX, MouseY float64

	Connected bool

	// Proposed skin/effect (submitted when joining)
	Skin   string
	Effect string

	// Vote choices (during voting phase)
	SkinVote   string
	EffectVote string
	HasVoted   bool

	// IsHost — first member; breaks tie votes
	IsHost bool
}

// TankSession manages a single tank instance (matchmaking through gameplay).
type TankSession struct {
	ID          string
	State       TankState
	DesiredSize int  // 2, 3, or 4
	Private     bool // private (invite-code) or public (auto-match)
	Code        string

	mu      sync.Mutex
	Members []*TankMember

	// Shared Player created at game start
	Player *Player

	// Timers
	CreatedAt time.Time
	VoteStart time.Time
	WaitSecs  int // 60 second public wait, configurable

	// Callback set by the api layer to push state to connected members
	OnStateChange func(session *TankSession)
}

// Lock acquires the session lock (for external callers that need atomic access to Members).
func (ts *TankSession) Lock() { ts.mu.Lock() }

// Unlock releases the session lock.
func (ts *TankSession) Unlock() { ts.mu.Unlock() }

// NewTankSession creates a new tank session.
func NewTankSession(desiredSize int, private bool) *TankSession {
	code := generateCode(6)
	return &TankSession{
		ID:          code,
		State:       TankWaiting,
		DesiredSize: desiredSize,
		Private:     private,
		Code:        code,
		Members:     make([]*TankMember, 0, 4),
		CreatedAt:   time.Now(),
		WaitSecs:    60,
	}
}

// AddMember adds a member to the session. Returns false if session is full or not in waiting state.
func (ts *TankSession) AddMember(m *TankMember) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.State != TankWaiting {
		return false
	}
	if len(ts.Members) >= ts.DesiredSize {
		return false
	}

	if len(ts.Members) == 0 {
		m.IsHost = true
	}
	m.Connected = true
	ts.Members = append(ts.Members, m)

	// If we have enough members, transition to voting
	if len(ts.Members) >= ts.DesiredSize {
		ts.State = TankVoting
		ts.VoteStart = time.Now()
	}
	return true
}

// RemoveMember removes a member by ConnID. Returns true if found and removed.
// In waiting state, this removes the member. In playing state, marks disconnected.
func (ts *TankSession) RemoveMember(connID interface{}) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	for i, m := range ts.Members {
		if m.ConnID == connID {
			switch ts.State {
			case TankWaiting, TankVoting:
				// Remove entirely
				ts.Members = append(ts.Members[:i], ts.Members[i+1:]...)
				// If host left, reassign
				if m.IsHost && len(ts.Members) > 0 {
					ts.Members[0].IsHost = true
				}
				// If we were voting and someone left, go back to waiting
				if ts.State == TankVoting {
					ts.State = TankWaiting
				}
				return true
			case TankPlaying:
				// Mark disconnected but keep in the list
				m.Connected = false
				return true
			}
		}
	}
	return false
}

// Vote records a member's skin/effect vote.
func (ts *TankSession) Vote(connID interface{}, skin, effect string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.State != TankVoting {
		return false
	}

	for _, m := range ts.Members {
		if m.ConnID == connID {
			m.SkinVote = skin
			m.EffectVote = effect
			m.HasVoted = true
			return true
		}
	}
	return false
}

// AllVoted returns true if every member has cast a vote.
func (ts *TankSession) AllVoted() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, m := range ts.Members {
		if !m.HasVoted {
			return false
		}
	}
	return true
}

// VoteTimeExpired returns true if 10 seconds have passed since voting started.
func (ts *TankSession) VoteTimeExpired() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return time.Since(ts.VoteStart) >= 10*time.Second
}

// WaitTimeExpired returns true if the waiting timer has expired.
func (ts *TankSession) WaitTimeExpired() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return time.Since(ts.CreatedAt) >= time.Duration(ts.WaitSecs)*time.Second
}

// WaitSecondsRemaining returns how many seconds remain in the wait timer.
func (ts *TankSession) WaitSecondsRemaining() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.WaitSecondsRemainingLocked()
}

// WaitSecondsRemainingLocked returns wait seconds remaining (caller must hold lock).
func (ts *TankSession) WaitSecondsRemainingLocked() int {
	elapsed := int(time.Since(ts.CreatedAt).Seconds())
	rem := ts.WaitSecs - elapsed
	if rem < 0 {
		rem = 0
	}
	return rem
}

// VoteSecondsRemaining returns how many seconds remain in the vote timer.
func (ts *TankSession) VoteSecondsRemaining() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.VoteSecondsRemainingLocked()
}

// VoteSecondsRemainingLocked returns vote seconds remaining (caller must hold lock).
func (ts *TankSession) VoteSecondsRemainingLocked() int {
	elapsed := int(time.Since(ts.VoteStart).Seconds())
	rem := 10 - elapsed
	if rem < 0 {
		rem = 0
	}
	return rem
}

// ConnectedCount returns the number of currently connected members.
func (ts *TankSession) ConnectedCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	for _, m := range ts.Members {
		if m.Connected {
			n++
		}
	}
	return n
}

// ResolveVotes determines the winning skin and effect.
// Majority wins; host's choice breaks ties.
func (ts *TankSession) ResolveVotes() (skin string, effect string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	skinTally := map[string]int{}
	effectTally := map[string]int{}
	var hostSkinVote, hostEffectVote string

	for _, m := range ts.Members {
		sv := m.SkinVote
		ev := m.EffectVote
		// Default to member's proposed skin/effect if they didn't vote
		if sv == "" {
			sv = m.Skin
		}
		if ev == "" {
			ev = m.Effect
		}
		skinTally[sv]++
		effectTally[ev]++
		if m.IsHost {
			hostSkinVote = sv
			hostEffectVote = ev
		}
	}

	skin = resolveTally(skinTally, hostSkinVote)
	effect = resolveTally(effectTally, hostEffectVote)
	return
}

// CombinedName builds the display name for the tank: "Name1 · Name2 · Name3"
func (ts *TankSession) CombinedName() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	names := make([]string, 0, len(ts.Members))
	for _, m := range ts.Members {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return "Tank"
	}
	return strings.Join(names, " · ")
}

// AverageMousePosition returns the mean mouse position of all connected members.
func (ts *TankSession) AverageMousePosition() (float64, float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var sumX, sumY float64
	n := 0
	for _, m := range ts.Members {
		if m.Connected {
			sumX += m.MouseX
			sumY += m.MouseY
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sumX / float64(n), sumY / float64(n)
}

// UpdateMemberMouse updates a specific member's mouse position and recalculates the average.
func (ts *TankSession) UpdateMemberMouse(connID interface{}, x, y float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, m := range ts.Members {
		if m.ConnID == connID {
			m.MouseX = x
			m.MouseY = y
			break
		}
	}
	// Recalculate average
	var sumX, sumY float64
	n := 0
	for _, m := range ts.Members {
		if m.Connected {
			sumX += m.MouseX
			sumY += m.MouseY
			n++
		}
	}
	if n > 0 && ts.Player != nil {
		ts.Player.SetMouse(sumX/float64(n), sumY/float64(n))
	}
}

// GetMemberMouse returns the individual mouse position of a specific member.
func (ts *TankSession) GetMemberMouse(connID interface{}) (float64, float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, m := range ts.Members {
		if m.ConnID == connID {
			return m.MouseX, m.MouseY
		}
	}
	return 0, 0
}

// TransitionToPlaying sets the state to Playing and creates the shared Player.
func (ts *TankSession) TransitionToPlaying(skin, effect string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.State = TankPlaying
	name := ts.combinedNameLocked()
	p := NewPlayer(name, skin, effect)
	p.IsTank = true
	p.TankMemberCount = len(ts.Members)
	ts.Player = p
}

// End marks the session as ended.
func (ts *TankSession) End() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.State = TankEnded
}

// ── helpers ──

func (ts *TankSession) combinedNameLocked() string {
	names := make([]string, 0, len(ts.Members))
	for _, m := range ts.Members {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return "Tank"
	}
	return strings.Join(names, " · ")
}

func resolveTally(tally map[string]int, hostVote string) string {
	if len(tally) == 0 {
		return ""
	}
	bestCount := 0
	bestVal := ""
	for val, count := range tally {
		if count > bestCount || (count == bestCount && val == hostVote) {
			bestCount = count
			bestVal = val
		}
	}
	return bestVal
}

func generateCode(n int) string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // exclude 0/O/I/1 for clarity
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// ────────────────────────────────────────────────────────────
// TankManager manages all active tank sessions and matchmaking.
// ────────────────────────────────────────────────────────────

type TankManager struct {
	mu       sync.Mutex
	sessions map[string]*TankSession // keyed by session ID / code

	// Public matchmaking queue: desiredSize → list of waiting sessions
	publicQueue map[int][]*TankSession
}

// NewTankManager creates a new TankManager.
func NewTankManager() *TankManager {
	return &TankManager{
		sessions:    make(map[string]*TankSession),
		publicQueue: make(map[int][]*TankSession),
	}
}

// CreatePrivate creates a new private tank session.
func (tm *TankManager) CreatePrivate(desiredSize int, member *TankMember) *TankSession {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	session := NewTankSession(desiredSize, true)
	session.AddMember(member)
	tm.sessions[session.Code] = session
	return session
}

// JoinPrivate joins an existing private session by code.
func (tm *TankManager) JoinPrivate(code string, member *TankMember) *TankSession {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	session, ok := tm.sessions[code]
	if !ok {
		return nil
	}
	if !session.AddMember(member) {
		return nil
	}
	return session
}

// QueuePublic finds or creates a public session for the given size and adds the member.
func (tm *TankManager) QueuePublic(desiredSize int, member *TankMember) *TankSession {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Try to find an existing waiting public session with matching size
	candidates := tm.publicQueue[desiredSize]
	for i, session := range candidates {
		if session.State == TankWaiting && session.AddMember(member) {
			// If session filled up, remove from queue
			if session.State == TankVoting {
				tm.publicQueue[desiredSize] = append(candidates[:i], candidates[i+1:]...)
			}
			return session
		}
	}

	// Also check "don't mind" queue (size 0) — put this member in matching sessions
	if desiredSize != 0 {
		for i, session := range tm.publicQueue[0] {
			if session.State == TankWaiting && session.AddMember(member) {
				if session.State == TankVoting {
					tm.publicQueue[0] = append(tm.publicQueue[0][:i], tm.publicQueue[0][i+1:]...)
				}
				return session
			}
		}
	}

	// No match found — create a new public session
	session := NewTankSession(desiredSize, false)
	session.AddMember(member)
	tm.sessions[session.Code] = session
	tm.publicQueue[desiredSize] = append(tm.publicQueue[desiredSize], session)
	return session
}

// RemoveMember removes a member from their session. Cleans up empty sessions.
func (tm *TankManager) RemoveMemberFromSession(connID interface{}, sessionCode string) {
	tm.mu.Lock()
	session, ok := tm.sessions[sessionCode]
	tm.mu.Unlock()

	if !ok {
		return
	}

	session.RemoveMember(connID)

	// Clean up empty sessions
	if session.ConnectedCount() == 0 {
		tm.mu.Lock()
		delete(tm.sessions, sessionCode)
		// Remove from public queue
		for size, q := range tm.publicQueue {
			for i, s := range q {
				if s.Code == sessionCode {
					tm.publicQueue[size] = append(q[:i], q[i+1:]...)
					break
				}
			}
		}
		tm.mu.Unlock()
	}
}

// GetSession returns a session by code.
func (tm *TankManager) GetSession(code string) *TankSession {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.sessions[code]
}

// CleanupExpired removes expired sessions (waiting timer expired with < desiredSize players).
// Called periodically from the game loop.
func (tm *TankManager) CleanupExpired() []*TankSession {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var expired []*TankSession
	for code, session := range tm.sessions {
		if session.State == TankWaiting && session.WaitTimeExpired() {
			session.mu.Lock()
			session.State = TankEnded
			session.mu.Unlock()
			expired = append(expired, session)
			delete(tm.sessions, code)
			// Remove from public queue
			for size, q := range tm.publicQueue {
				for i, s := range q {
					if s.Code == code {
						tm.publicQueue[size] = append(q[:i], q[i+1:]...)
						break
					}
				}
			}
		}
		if session.State == TankEnded {
			delete(tm.sessions, code)
		}
	}
	return expired
}

// RemoveSession deletes a session by code.
func (tm *TankManager) RemoveSession(code string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.sessions, code)
	for size, q := range tm.publicQueue {
		for i, s := range q {
			if s.Code == code {
				tm.publicQueue[size] = append(q[:i], q[i+1:]...)
				break
			}
		}
	}
}
