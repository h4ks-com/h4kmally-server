package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ── Rate Limiter ─────────────────────────────────────────────

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     int           // max requests per window
	window   time.Duration // rolling window
}

type visitor struct {
	timestamps []time.Time
}

func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		window:   window,
	}
	// Periodically clean up stale entries
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.cleanup()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, ok := rl.visitors[ip]
	if !ok {
		v = &visitor{}
		rl.visitors[ip] = v
	}

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Remove timestamps older than the window
	valid := v.timestamps[:0]
	for _, t := range v.timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	v.timestamps = valid

	if len(v.timestamps) >= rl.rate {
		return false
	}
	v.timestamps = append(v.timestamps, now)
	return true
}

func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-rl.window)
	for ip, v := range rl.visitors {
		valid := v.timestamps[:0]
		for _, t := range v.timestamps {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(rl.visitors, ip)
		} else {
			v.timestamps = valid
		}
	}
}

// ── Public Status API ────────────────────────────────────────

// statusLimiter allows 30 requests per minute per IP.
var statusLimiter = newRateLimiter(30, time.Minute)

type StatusPlayerEntry struct {
	Name      string `json:"name"`
	Skin      string `json:"skin,omitempty"`
	Effect    string `json:"effect,omitempty"`
	Score     int    `json:"score"`
	Cells     int    `json:"cells"`
	IsBot     bool   `json:"isBot"`
	Clan      string `json:"clan,omitempty"`
}

type StatusResponse struct {
	PlayerCount    int                 `json:"playerCount"`
	BotCount       int                 `json:"botCount"`
	SpectatorCount int                 `json:"spectatorCount"`
	Players        []StatusPlayerEntry `json:"players"`
	Bots           []StatusPlayerEntry `json:"bots"`
	Spectators     []StatusSpectator   `json:"spectators"`
}

type StatusSpectator struct {
	Name string `json:"name,omitempty"`
}

// HandleStatus serves GET /api/status — public server overview.
func (s *Server) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Rate limit by IP
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = fwd
	}
	if !statusLimiter.allow(ip) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Gather all engine players (includes bots)
	allPlayers := s.Engine.GetPlayers()

	// Build a set of player IDs owned by real clients (non-bots)
	realPlayerIDs := make(map[uint32]bool)
	s.mu.RLock()
	for client := range s.clients {
		if client.player != nil {
			realPlayerIDs[client.player.ID] = true
		}
		client.mu.Lock()
		if client.multiPlayer != nil {
			realPlayerIDs[client.multiPlayer.ID] = true
		}
		client.mu.Unlock()
	}
	s.mu.RUnlock()

	var players []StatusPlayerEntry
	var bots []StatusPlayerEntry
	for _, p := range allPlayers {
		if !p.Alive {
			continue
		}
		entry := StatusPlayerEntry{
			Name:   p.Name,
			Skin:   p.Skin,
			Effect: p.Effect,
			Score:  int(p.Score),
			Cells:  len(p.Cells),
			Clan:   p.Clan,
		}
		if realPlayerIDs[p.ID] {
			entry.IsBot = false
			players = append(players, entry)
		} else {
			entry.IsBot = true
			bots = append(bots, entry)
		}
	}

	// Spectators: clients that are spectating
	var spectators []StatusSpectator
	s.mu.RLock()
	for client := range s.clients {
		client.mu.Lock()
		isSpec := client.spectating
		client.mu.Unlock()
		if isSpec {
			name := ""
			if client.player != nil {
				name = client.player.Name
			}
			spectators = append(spectators, StatusSpectator{Name: name})
		}
	}
	s.mu.RUnlock()

	if players == nil {
		players = []StatusPlayerEntry{}
	}
	if bots == nil {
		bots = []StatusPlayerEntry{}
	}
	if spectators == nil {
		spectators = []StatusSpectator{}
	}

	resp := StatusResponse{
		PlayerCount:    len(players),
		BotCount:       len(bots),
		SpectatorCount: len(spectators),
		Players:        players,
		Bots:           bots,
		Spectators:     spectators,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
