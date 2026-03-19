package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuthSession represents a validated user session.
type AuthSession struct {
	UserSub      string
	UserName     string
	UserUsername string // Logto username (for payment systems)
	UserPic      string
	ExpiresAt    time.Time
}

// AuthManager handles Logto OAuth2 token validation and session management.
type AuthManager struct {
	sessions      map[string]*AuthSession
	mu            sync.RWMutex
	logtoEndpoint string // e.g. "https://auth.h4ks.com"
	UserStore     *UserStore
}

// NewAuthManager creates an AuthManager that validates tokens against the given Logto endpoint.
func NewAuthManager(logtoEndpoint string, userStore *UserStore) *AuthManager {
	am := &AuthManager{
		sessions:      make(map[string]*AuthSession),
		logtoEndpoint: strings.TrimRight(logtoEndpoint, "/"),
		UserStore:     userStore,
	}

	// Periodically clean up expired sessions
	go func() {
		for range time.NewTicker(5 * time.Minute).C {
			am.cleanup()
		}
	}()

	return am
}

func (am *AuthManager) cleanup() {
	am.mu.Lock()
	defer am.mu.Unlock()
	now := time.Now()
	for token, session := range am.sessions {
		if now.After(session.ExpiresAt) {
			delete(am.sessions, token)
		}
	}
}

func generateSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleAuthMe validates a Logto access token via the userinfo endpoint
// and returns the user's game profile + a session token.
//
// Request:  GET /api/auth/me  (Authorization: Bearer <logto_access_token>)
// Response: { "session": "...", "user": { ... } }
func (am *AuthManager) HandleAuthMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	// Extract Bearer token
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"missing bearer token"}`))
		return
	}
	accessToken := auth[7:]

	// Validate by calling Logto's userinfo endpoint
	req, err := http.NewRequest("GET", am.logtoEndpoint+"/oidc/me", nil)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Logto userinfo request failed: %v", err)
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"failed to validate token"}`))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Logto userinfo returned %d: %s", resp.StatusCode, string(body))
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid token"}`))
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"failed to read response"}`))
		return
	}

	var userInfo struct {
		Sub      string `json:"sub"`
		Name     string `json:"name"`
		Username string `json:"username"`
		Picture  string `json:"picture"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(body, &userInfo); err != nil {
		log.Printf("Failed to parse userinfo JSON: %v — raw: %s", err, string(body))
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"invalid userinfo response"}`))
		return
	}

	log.Printf("Logto userinfo: sub=%q name=%q username=%q email=%q", userInfo.Sub, userInfo.Name, userInfo.Username, userInfo.Email)

	if userInfo.Sub == "" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid user info: missing sub"}`))
		return
	}

	// Use best available display name: name > username > email
	displayName := userInfo.Name
	if displayName == "" {
		displayName = userInfo.Username
	}
	if displayName == "" {
		displayName = userInfo.Email
	}

	// Create/update user in store
	user := am.UserStore.GetOrCreate(userInfo.Sub, displayName, userInfo.Picture)

	// Create session
	sessionToken := generateSessionToken()
	am.mu.Lock()
	am.sessions[sessionToken] = &AuthSession{
		UserSub:      userInfo.Sub,
		UserName:     displayName,
		UserUsername: userInfo.Username,
		UserPic:      userInfo.Picture,
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	am.mu.Unlock()

	log.Printf("User authenticated: sub=%s name=%q", userInfo.Sub, userInfo.Name)

	// Return response
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session": sessionToken,
		"user":    user,
	})
}

// HandleAuthProfile returns the user profile for a given session token.
//
// Request:  GET /api/auth/profile?session=<token>
// Response: { "user": { ... } }
func (am *AuthManager) HandleAuthProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	sessionToken := r.URL.Query().Get("session")
	session := am.ValidateSession(sessionToken)
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid or expired session"}`))
		return
	}

	user := am.UserStore.Get(session.UserSub)
	if user == nil {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	level := LevelFromPoints(user.Points)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"user":      user,
		"level":     level,
		"xpCurrent": user.Points - XPForLevel(level),
		"xpNeeded":  XPForLevel(level+1) - XPForLevel(level),
	})
}

// HandleTokenReveal clears the user's pending token list (after they've been shown the reveal UI).
// POST /api/auth/tokens/reveal?session=TOKEN
func (am *AuthManager) HandleTokenReveal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}
	sessionToken := r.URL.Query().Get("session")
	session := am.ValidateSession(sessionToken)
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid or expired session"}`))
		return
	}
	am.UserStore.RevealTokens(session.UserSub)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// ValidateSession checks if a session token is valid and not expired.
func (am *AuthManager) ValidateSession(token string) *AuthSession {
	if token == "" {
		return nil
	}
	am.mu.RLock()
	defer am.mu.RUnlock()
	session := am.sessions[token]
	if session == nil {
		return nil
	}
	if time.Now().After(session.ExpiresAt) {
		return nil
	}
	return session
}
