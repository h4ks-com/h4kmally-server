package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// AdminHandler provides admin API endpoints.
type AdminHandler struct {
	server  *Server
	authMgr *AuthManager
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(server *Server, authMgr *AuthManager) *AdminHandler {
	return &AdminHandler{
		server:  server,
		authMgr: authMgr,
	}
}

// requireAdmin validates the session and checks admin status.
// Returns the session or writes an error response.
func (ah *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) *AuthSession {
	sessionToken := r.URL.Query().Get("session")
	if sessionToken == "" {
		// Try Authorization header
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			sessionToken = auth[7:]
		}
	}

	session := ah.authMgr.ValidateSession(sessionToken)
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return nil
	}

	if !ah.authMgr.UserStore.IsAdmin(session.UserSub) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"admin access required"}`))
		return nil
	}

	return session
}

// HandleAdminUsers lists all registered users.
// GET /api/admin/users?session=<token>
func (ah *AdminHandler) HandleAdminUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if ah.requireAdmin(w, r) == nil {
		return
	}

	users := ah.authMgr.UserStore.GetAll()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"users": users,
	})
}

// OnlinePlayerInfo represents info about an online player for the admin panel.
type OnlinePlayerInfo struct {
	PlayerID uint32  `json:"playerId"`
	Name     string  `json:"name"`
	Skin     string  `json:"skin"`
	Score    float64 `json:"score"`
	Alive    bool    `json:"alive"`
	UserSub  string  `json:"userSub,omitempty"`
	IP       string  `json:"ip,omitempty"`
	CenterX  float64 `json:"centerX"`
	CenterY  float64 `json:"centerY"`
}

// HandleAdminOnline lists all currently connected players.
// GET /api/admin/online?session=<token>
func (ah *AdminHandler) HandleAdminOnline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if ah.requireAdmin(w, r) == nil {
		return
	}

	ah.server.mu.RLock()
	var players []OnlinePlayerInfo
	for client := range ah.server.clients {
		if client.player == nil {
			continue
		}
		cx, cy := client.player.Center()
		ip := ""
		if client.conn != nil {
			ip = extractIP(client.conn.RemoteAddr().String())
		}
		players = append(players, OnlinePlayerInfo{
			PlayerID: client.player.ID,
			Name:     client.player.Name,
			Skin:     client.player.Skin,
			Score:    client.player.Score,
			Alive:    client.player.Alive,
			UserSub:  client.userSub,
			IP:       ip,
			CenterX:  cx,
			CenterY:  cy,
		})
	}
	ah.server.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"players": players,
	})
}

// HandleAdminSetAdmin grants or revokes admin status.
// POST /api/admin/set-admin?session=<token>  body: {"sub":"...", "isAdmin": true}
func (ah *AdminHandler) HandleAdminSetAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	session := ah.requireAdmin(w, r)
	if session == nil {
		return
	}

	// Only super admin can set admins
	if !ah.authMgr.UserStore.IsSuperAdmin(session.UserSub) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"only super admin can assign admins"}`))
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		Sub     string `json:"sub"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Sub == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	if !ah.authMgr.UserStore.SetAdmin(req.Sub, req.IsAdmin) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminBanUser bans a user by account sub.
// POST /api/admin/ban-user?session=<token>  body: {"sub":"...", "duration":-1, "reason":"..."}
// duration: -1 = permanent, or unix timestamp when ban expires
func (ah *AdminHandler) HandleAdminBanUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	session := ah.requireAdmin(w, r)
	if session == nil {
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		Sub      string `json:"sub"`
		Duration int64  `json:"duration"` // -1 = permanent, else unix timestamp
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Sub == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	// Cannot ban another admin unless super admin
	targetUser := ah.authMgr.UserStore.Get(req.Sub)
	if targetUser != nil && targetUser.IsAdmin && !ah.authMgr.UserStore.IsSuperAdmin(session.UserSub) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"cannot ban an admin"}`))
		return
	}

	if !ah.authMgr.UserStore.BanUser(req.Sub, req.Duration, req.Reason) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	// Kick the banned user if they're connected
	ah.server.KickUserSub(req.Sub, "You have been banned: "+req.Reason)

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminUnbanUser unbans a user.
// POST /api/admin/unban-user?session=<token>  body: {"sub":"..."}
func (ah *AdminHandler) HandleAdminUnbanUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if ah.requireAdmin(w, r) == nil {
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Sub == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	ah.authMgr.UserStore.UnbanUser(req.Sub)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminBanIP bans an IP address.
// POST /api/admin/ban-ip?session=<token>  body: {"ip":"...", "reason":"...", "expiresAt":-1}
func (ah *AdminHandler) HandleAdminBanIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	session := ah.requireAdmin(w, r)
	if session == nil {
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		IP        string `json:"ip"`
		Reason    string `json:"reason"`
		ExpiresAt int64  `json:"expiresAt"` // -1 = permanent
	}
	if err := json.Unmarshal(body, &req); err != nil || req.IP == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	ah.authMgr.UserStore.BanIP(req.IP, req.Reason, session.UserSub, req.ExpiresAt)

	// Kick all clients from that IP
	ah.server.KickIP(req.IP, "Your IP has been banned: "+req.Reason)

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminUnbanIP unbans an IP.
// POST /api/admin/unban-ip?session=<token>  body: {"ip":"..."}
func (ah *AdminHandler) HandleAdminUnbanIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if ah.requireAdmin(w, r) == nil {
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.IP == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request"}`))
		return
	}

	ah.authMgr.UserStore.UnbanIP(req.IP)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminIPBans lists all IP bans.
// GET /api/admin/ip-bans?session=<token>
func (ah *AdminHandler) HandleAdminIPBans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if ah.requireAdmin(w, r) == nil {
		return
	}

	bans := ah.authMgr.UserStore.GetIPBans()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"bans": bans,
	})
}

// extractIP strips the port from a host:port address.
func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// ── Skin Management ──────────────────────────────────────────

type skinEntry struct {
	Name     string `json:"name"`
	File     string `json:"file"`
	Category string `json:"category"`
	Rarity   string `json:"rarity"`
	MinLevel int    `json:"minLevel,omitempty"` // for "level" category skins
	OwnerSub string `json:"ownerSub,omitempty"` // for "custom" category — owner's user sub
}

const skinsDir = "skins"
const manifestPath = "skins/manifest.json"

func loadManifest() ([]skinEntry, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var skins []skinEntry
	if err := json.Unmarshal(data, &skins); err != nil {
		return nil, err
	}
	return skins, nil
}

func saveManifest(skins []skinEntry) error {
	data, err := json.MarshalIndent(skins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, data, 0644)
}

// HandleAdminSkins lists all skins.
// GET /api/admin/skins
func (ah *AdminHandler) HandleAdminSkins(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if ah.requireAdmin(w, r) == nil {
		return
	}
	skins, err := loadManifest()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to load manifest: " + err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"skins": skins})
}

// HandleAdminUploadSkin uploads a new skin image and adds it to the manifest.
// POST /api/admin/upload-skin (multipart/form-data: file, name, category, rarity)
func (ah *AdminHandler) HandleAdminUploadSkin(w http.ResponseWriter, r *http.Request) {
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
	if ah.requireAdmin(w, r) == nil {
		return
	}

	// Parse multipart form (max 10MB)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data: " + err.Error()})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing file field"})
		return
	}
	defer file.Close()

	name := r.FormValue("name")
	category := r.FormValue("category")
	rarity := r.FormValue("rarity")
	minLevelStr := r.FormValue("minLevel")

	if name == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"name is required"}`))
		return
	}
	if category == "" {
		category = "free"
	}
	if rarity == "" {
		rarity = "common"
	}
	var minLevel int
	if minLevelStr != "" {
		fmt.Sscanf(minLevelStr, "%d", &minLevel)
	}

	// Validate file extension
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid file type, must be png/jpg/gif/webp"}`))
		return
	}

	// Use provided name + original extension as filename
	fileName := name + ext
	destPath := filepath.Join(skinsDir, fileName)

	// Check if file already exists
	if _, err := os.Stat(destPath); err == nil {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("skin file %q already exists", fileName)})
		return
	}

	// Write file to disk
	dst, err := os.Create(destPath)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to create file: " + err.Error()})
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		os.Remove(destPath)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to write file: " + err.Error()})
		return
	}

	// Add to manifest
	skins, err := loadManifest()
	if err != nil {
		skins = []skinEntry{}
	}
	skins = append(skins, skinEntry{
		Name:     name,
		File:     fileName,
		Category: category,
		Rarity:   rarity,
		MinLevel: minLevel,
	})
	if err := saveManifest(skins); err != nil {
		// Remove the uploaded file if manifest save fails
		os.Remove(destPath)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to update manifest: " + err.Error()})
		return
	}

	log.Printf("Admin uploaded skin: %s (%s, %s)", name, category, rarity)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":   true,
		"skin": skins[len(skins)-1],
	})
}

// HandleAdminDeleteSkin deletes a skin image and removes it from the manifest.
// POST /api/admin/delete-skin { "name": "SkinName" }
func (ah *AdminHandler) HandleAdminDeleteSkin(w http.ResponseWriter, r *http.Request) {
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
	if ah.requireAdmin(w, r) == nil {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"name is required"}`))
		return
	}

	skins, err := loadManifest()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to load manifest"})
		return
	}

	// Find and remove from manifest
	found := false
	var fileName string
	var deletedOwnerSub string
	var remaining []skinEntry
	for _, s := range skins {
		if s.Name == req.Name {
			found = true
			fileName = s.File
			deletedOwnerSub = s.OwnerSub
		} else {
			remaining = append(remaining, s)
		}
	}

	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("skin %q not found", req.Name)})
		return
	}

	// Save updated manifest
	if err := saveManifest(remaining); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to update manifest"})
		return
	}

	// Delete image file
	if fileName != "" {
		os.Remove(filepath.Join(skinsDir, fileName))
	}

	// If this was a custom skin, clean up the owner's profile and restore their slot
	if deletedOwnerSub != "" {
		ah.authMgr.UserStore.RemoveCustomSkin(deletedOwnerSub, req.Name)
		log.Printf("Admin deleted custom skin: %s (owner: %s)", req.Name, deletedOwnerSub)
	} else {
		log.Printf("Admin deleted skin: %s", req.Name)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// HandleAdminSetSkinLevel sets the minimum level for a level-category skin.
// POST /api/admin/set-skin-level { "name": "SkinName", "minLevel": 5 }
func (ah *AdminHandler) HandleAdminSetSkinLevel(w http.ResponseWriter, r *http.Request) {
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
	if ah.requireAdmin(w, r) == nil {
		return
	}

	var req struct {
		Name     string `json:"name"`
		MinLevel int    `json:"minLevel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"name is required"}`))
		return
	}

	skins, err := loadManifest()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to load manifest"})
		return
	}

	found := false
	for i := range skins {
		if skins[i].Name == req.Name {
			skins[i].MinLevel = req.MinLevel
			found = true
			break
		}
	}
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("skin %q not found", req.Name)})
		return
	}

	if err := saveManifest(skins); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to save manifest"})
		return
	}

	log.Printf("Admin set skin %q minLevel to %d", req.Name, req.MinLevel)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// GetPremiumSkinNames returns the names of all premium-category skins.
func GetPremiumSkinNames() []string {
	skins, err := loadManifest()
	if err != nil {
		return nil
	}
	var names []string
	for _, s := range skins {
		if s.Category == "premium" {
			names = append(names, s.Name)
		}
	}
	return names
}

// HandleAdminBRStart starts a Battle Royale event.
// POST /api/admin/br/start
func (ah *AdminHandler) HandleAdminBRStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	sess := ah.requireAdmin(w, r)
	if sess == nil {
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}

	br := ah.server.BattleRoyale
	if br == nil {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"battle royale not initialized"}`))
		return
	}

	mapHalfW := ah.server.Engine.Cfg.MapWidth
	br.Start(mapHalfW)
	w.Write([]byte(`{"status":"started"}`))
}

// HandleAdminBRStop stops the current Battle Royale event.
// POST /api/admin/br/stop
func (ah *AdminHandler) HandleAdminBRStop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	sess := ah.requireAdmin(w, r)
	if sess == nil {
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}

	br := ah.server.BattleRoyale
	if br == nil {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"battle royale not initialized"}`))
		return
	}

	br.Stop()
	ah.server.BroadcastBattleRoyale() // send state=0 to clear clients
	w.Write([]byte(`{"status":"stopped"}`))
}

// HandleAdminBRStatus returns the current Battle Royale status.
// GET /api/admin/br/status
func (ah *AdminHandler) HandleAdminBRStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	sess := ah.requireAdmin(w, r)
	if sess == nil {
		return
	}

	br := ah.server.BattleRoyale
	if br == nil {
		w.Write([]byte(`{"state":0}`))
		return
	}

	info := br.GetInfo()
	json.NewEncoder(w).Encode(info)
}

// HandleAdminGrantPowerup grants a specific powerup to a user.
// POST /api/admin/grant-powerup  body: {"sub":"...","powerup":"virus_layer","charges":5}
func (ah *AdminHandler) HandleAdminGrantPowerup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	sess := ah.requireAdmin(w, r)
	if sess == nil {
		return
	}

	var body struct {
		Sub     string `json:"sub"`
		Powerup string `json:"powerup"`
		Charges int    `json:"charges"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid json"}`))
		return
	}

	// Validate powerup type
	pType := PowerupType(body.Powerup)
	pDef := GetPowerupDef(pType)
	if pDef == nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid powerup type"}`))
		return
	}

	charges := body.Charges
	if charges <= 0 {
		charges = pDef.Charges // default charges
	}

	ok := ah.authMgr.UserStore.AdminGrantPowerup(body.Sub, pType, charges)
	if !ok {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	log.Printf("[Admin] %s granted powerup %s (%d charges) to %s", sess.UserSub, body.Powerup, charges, body.Sub)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"powerup": body.Powerup,
		"charges": charges,
	})
}
