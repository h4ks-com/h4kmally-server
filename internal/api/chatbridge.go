package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/h4ks-com/h4kmally-server/internal/game"
	"github.com/h4ks-com/h4kmally-server/internal/protocol"
)

// ChatBridge handles bidirectional chat between the game server and external services.
type ChatBridge struct {
	server     *Server
	token      string // required bearer token for incoming messages
	webhookURL string // outgoing webhook URL (empty = disabled)
	limiter    *rateLimiter
	httpClient *http.Client
}

// NewChatBridge creates a new chat bridge.
// token is required for incoming auth; webhookURL can be empty to disable outgoing.
func NewChatBridge(server *Server, token, webhookURL string) *ChatBridge {
	return &ChatBridge{
		server:     server,
		token:      token,
		webhookURL: webhookURL,
		limiter:    newRateLimiter(10, time.Minute), // 10 messages/min per IP
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// ── Incoming: external → game chat ───────────────────────────

type IncomingChatRequest struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Color   [3]int `json:"color,omitempty"` // optional RGB, defaults to random
}

type IncomingChatResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// HandleIncoming handles POST /api/chat/send — authenticated endpoint for external messages.
func (cb *ChatBridge) HandleIncoming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != cb.token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(IncomingChatResponse{OK: false, Error: "invalid or missing bearer token"})
		return
	}

	// Rate limit
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = fwd
	}
	if !cb.limiter.allow(ip) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(IncomingChatResponse{OK: false, Error: "rate limit exceeded"})
		return
	}

	// Parse body
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(IncomingChatResponse{OK: false, Error: "bad request body"})
		return
	}

	var req IncomingChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(IncomingChatResponse{OK: false, Error: "invalid JSON"})
		return
	}

	if req.Name == "" || req.Message == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(IncomingChatResponse{OK: false, Error: "name and message are required"})
		return
	}

	// Truncate
	if len(req.Name) > 50 {
		req.Name = req.Name[:50]
	}
	if len(req.Message) > 200 {
		req.Message = req.Message[:200]
	}

	// Color: use provided or random
	var r0, g0, b0 uint8
	if req.Color[0] > 0 || req.Color[1] > 0 || req.Color[2] > 0 {
		r0 = uint8(req.Color[0] & 0xFF)
		g0 = uint8(req.Color[1] & 0xFF)
		b0 = uint8(req.Color[2] & 0xFF)
	} else {
		r0 = uint8(100 + rand.IntN(156))
		g0 = uint8(100 + rand.IntN(156))
		b0 = uint8(100 + rand.IntN(156))
	}

	// Broadcast to all clients
	cb.server.mu.RLock()
	for client := range cb.server.clients {
		if client.shuffle == nil {
			continue
		}
		msg := protocol.BuildChat(client.shuffle, 0, r0, g0, b0, req.Name, req.Message)
		client.sendMsg(msg)
	}
	cb.server.mu.RUnlock()

	log.Printf("Chat bridge incoming: [%s] %s", req.Name, req.Message)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(IncomingChatResponse{OK: true})
}

// ── Outgoing: game chat → external webhook ───────────────────

type OutgoingChatPayload struct {
	Name         string   `json:"name"`
	Message      string   `json:"message"`
	Color        [3]uint8 `json:"color"`
	IsSubscriber bool     `json:"isSubscriber"`
	Clan         string   `json:"clan,omitempty"`
}

// SendOutgoing posts a chat message to the configured webhook URL.
// Called from the server's BroadcastChat path. Non-blocking (fires in goroutine).
func (cb *ChatBridge) SendOutgoing(sender *game.Player, text string) {
	if cb.webhookURL == "" {
		return
	}

	payload := OutgoingChatPayload{
		Name:         sender.Name,
		Message:      text,
		Color:        sender.Color,
		IsSubscriber: sender.IsSubscriber,
		Clan:         sender.Clan,
	}

	go func() {
		body, err := json.Marshal(payload)
		if err != nil {
			log.Printf("Chat webhook marshal error: %v", err)
			return
		}
		req, err := http.NewRequest(http.MethodPost, cb.webhookURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("Chat webhook request error: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cb.token != "" {
			req.Header.Set("Authorization", "Bearer "+cb.token)
		}

		resp, err := cb.httpClient.Do(req)
		if err != nil {
			log.Printf("Chat webhook send error: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("Chat webhook returned %d", resp.StatusCode)
		}
	}()
}
