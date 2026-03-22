package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ── Clan Roles ─────────────────────────────────────────────

type ClanRole string

const (
	ClanRoleMember   ClanRole = "member"
	ClanRoleElder    ClanRole = "elder"
	ClanRoleCoLeader ClanRole = "co-leader"
	ClanRoleLeader   ClanRole = "leader"
)

// ClanRoleLevel returns the permission level of a role (higher = more perms).
func ClanRoleLevel(r ClanRole) int {
	switch r {
	case ClanRoleLeader:
		return 4
	case ClanRoleCoLeader:
		return 3
	case ClanRoleElder:
		return 2
	case ClanRoleMember:
		return 1
	}
	return 0
}

// ── Clan Data Structures ───────────────────────────────────

type ClanMember struct {
	Sub      string   `json:"sub"`
	Name     string   `json:"name"`
	Role     ClanRole `json:"role"`
	JoinedAt int64    `json:"joinedAt"`
}

type ClanJoinRequest struct {
	Sub         string `json:"sub"`
	Name        string `json:"name"`
	RequestedAt int64  `json:"requestedAt"`
}

type ClanSettings struct {
	AcceptingRequests bool   `json:"acceptingRequests"` // whether the clan accepts new join requests
	MinLevel          int    `json:"minLevel"`          // minimum player level to request joining
	Description       string `json:"description"`       // clan description / motto
	ClanColor         string `json:"clanColor"`         // hex color for tag display (e.g. "#ff5500")
	IsPublic          bool   `json:"isPublic"`          // visible in public clan browser
	MaxMembers        int    `json:"maxMembers"`        // max members allowed (default 50)
}

type Clan struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Tag          string            `json:"tag"` // short in-game tag, e.g. "H4K"
	CreatedAt    int64             `json:"createdAt"`
	CreatedBy    string            `json:"createdBy"`
	Members      []ClanMember      `json:"members"`
	JoinRequests []ClanJoinRequest `json:"joinRequests,omitempty"`
	Settings     ClanSettings      `json:"settings"`
}

// FindMember returns a pointer to the member with the given sub, or nil.
func (c *Clan) FindMember(sub string) *ClanMember {
	for i := range c.Members {
		if c.Members[i].Sub == sub {
			return &c.Members[i]
		}
	}
	return nil
}

// FindRequest returns the index of the join request with the given sub, or -1.
func (c *Clan) FindRequest(sub string) int {
	for i, r := range c.JoinRequests {
		if r.Sub == sub {
			return i
		}
	}
	return -1
}

// ── Clan Store ─────────────────────────────────────────────

type ClanStoreData struct {
	Clans map[string]*Clan `json:"clans"`
}

type ClanStore struct {
	mu    sync.RWMutex
	clans map[string]*Clan // clan ID → Clan
	path  string
}

func NewClanStore(path string) *ClanStore {
	cs := &ClanStore{
		clans: make(map[string]*Clan),
		path:  path,
	}
	cs.load()
	return cs
}

func (cs *ClanStore) load() {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		return
	}
	var store ClanStoreData
	if err := json.Unmarshal(data, &store); err == nil && store.Clans != nil {
		cs.clans = store.Clans
		log.Printf("Loaded %d clans from %s", len(cs.clans), cs.path)
	}
}

func (cs *ClanStore) Save() {
	cs.mu.RLock()
	data := ClanStoreData{Clans: cs.clans}
	cs.mu.RUnlock()
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("Error serializing clan data: %v", err)
		return
	}
	if err := os.WriteFile(cs.path, raw, 0644); err != nil {
		log.Printf("Error writing clan data: %v", err)
	}
}

func generateClanID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateClan creates a new clan and makes the given user the leader.
func (cs *ClanStore) CreateClan(name, tag, creatorSub, creatorName string) (*Clan, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Validate tag uniqueness
	tagLow := strings.ToLower(tag)
	for _, existing := range cs.clans {
		if strings.ToLower(existing.Tag) == tagLow {
			return nil, fmt.Errorf("a clan with tag '%s' already exists", tag)
		}
	}

	id := generateClanID()
	clan := &Clan{
		ID:        id,
		Name:      name,
		Tag:       tag,
		CreatedAt: time.Now().Unix(),
		CreatedBy: creatorSub,
		Members: []ClanMember{
			{Sub: creatorSub, Name: creatorName, Role: ClanRoleLeader, JoinedAt: time.Now().Unix()},
		},
		Settings: ClanSettings{
			AcceptingRequests: true,
			MinLevel:          1,
			IsPublic:          true,
			MaxMembers:        50,
			ClanColor:         "#ffffff",
		},
	}
	cs.clans[id] = clan
	return clan, nil
}

// GetClan returns a clan by ID or nil.
func (cs *ClanStore) GetClan(id string) *Clan {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.clans[id]
}

// ListPublicClans returns all public clans.
func (cs *ClanStore) ListPublicClans() []*Clan {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	var result []*Clan
	for _, c := range cs.clans {
		if c.Settings.IsPublic {
			result = append(result, c)
		}
	}
	return result
}

// FindClanByMember returns the clan the given user belongs to, or nil.
func (cs *ClanStore) FindClanByMember(sub string) *Clan {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.findClanByMemberLocked(sub)
}

// findClanByMemberLocked is FindClanByMember without locking (caller must hold lock).
func (cs *ClanStore) findClanByMemberLocked(sub string) *Clan {
	for _, c := range cs.clans {
		if c.FindMember(sub) != nil {
			return c
		}
	}
	return nil
}

// DeleteClan removes a clan entirely.
func (cs *ClanStore) DeleteClan(id string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.clans, id)
}

// ── Clan API Handler ───────────────────────────────────────

type ClanHandler struct {
	authMgr    *AuthManager
	userStore  *UserStore
	clanStore  *ClanStore
	payment    PaymentProvider                                  // may be nil
	clanChatFn func(clanID, senderName, senderSub, text string) // callback for clan chat broadcast
}

func NewClanHandler(authMgr *AuthManager, userStore *UserStore, clanStore *ClanStore, payment PaymentProvider) *ClanHandler {
	return &ClanHandler{
		authMgr:   authMgr,
		userStore: userStore,
		clanStore: clanStore,
		payment:   payment,
	}
}

// SetClanChatFn sets the callback for broadcasting clan chat messages.
func (ch *ClanHandler) SetClanChatFn(fn func(clanID, senderName, senderSub, text string)) {
	ch.clanChatFn = fn
}

// jsonError writes an error JSON response.
func clanError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// getSession validates the session from the Authorization header.
func (ch *ClanHandler) getSession(r *http.Request) *AuthSession {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return ch.authMgr.ValidateSession(token)
}

// HandleListClans lists all public clans.
// GET /api/clans
func (ch *ClanHandler) HandleListClans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	clans := ch.clanStore.ListPublicClans()

	type clanSummary struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Tag         string `json:"tag"`
		MemberCount int    `json:"memberCount"`
		Color       string `json:"color"`
		Description string `json:"description"`
		Accepting   bool   `json:"accepting"`
		MinLevel    int    `json:"minLevel"`
	}
	var result []clanSummary
	for _, c := range clans {
		result = append(result, clanSummary{
			ID:          c.ID,
			Name:        c.Name,
			Tag:         c.Tag,
			MemberCount: len(c.Members),
			Color:       c.Settings.ClanColor,
			Description: c.Settings.Description,
			Accepting:   c.Settings.AcceptingRequests,
			MinLevel:    c.Settings.MinLevel,
		})
	}
	if result == nil {
		result = []clanSummary{}
	}
	json.NewEncoder(w).Encode(result)
}

// HandleMyClan returns the current user's clan info.
// GET /api/clans/my
func (ch *ClanHandler) HandleMyClan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}
	clan := ch.clanStore.FindClanByMember(sess.UserSub)
	if clan == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"clan": nil})
		return
	}
	// Find the caller's role within the clan
	myRole := "member"
	for _, m := range clan.Members {
		if m.Sub == sess.UserSub {
			myRole = string(m.Role)
			break
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"clan": clan, "myRole": myRole})
}

// HandleClanDetail returns full clan info.
// GET /api/clans/detail?id=xxx
func (ch *ClanHandler) HandleClanDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	id := r.URL.Query().Get("id")
	clan := ch.clanStore.GetClan(id)
	if clan == nil {
		clanError(w, 404, "clan not found")
		return
	}
	json.NewEncoder(w).Encode(clan)
}

// HandleCreateClan creates a new clan.
// POST /api/clans/create  { "name": "...", "tag": "..." }
func (ch *ClanHandler) HandleCreateClan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	// Check user isn't already in a clan
	if existing := ch.clanStore.FindClanByMember(sess.UserSub); existing != nil {
		clanError(w, 400, "you are already in a clan")
		return
	}

	var body struct {
		Name string `json:"name"`
		Tag  string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		clanError(w, 400, "invalid request body")
		return
	}

	// Validate name and tag
	body.Name = strings.TrimSpace(body.Name)
	body.Tag = strings.TrimSpace(body.Tag)
	if body.Name == "" || utf8.RuneCountInString(body.Name) > 32 {
		clanError(w, 400, "clan name must be 1-32 characters")
		return
	}
	if body.Tag == "" || utf8.RuneCountInString(body.Tag) > 6 {
		clanError(w, 400, "clan tag must be 1-6 characters")
		return
	}

	// Admins create for free; others need 50 beans payment
	user := ch.userStore.GetUser(sess.UserSub)
	if user == nil || (!user.IsAdmin && ch.payment != nil) {
		// Non-admin: return payment URL for 50 beans
		// The client will redirect the user to pay, then call create again after payment
		// For now, we generate a payment URL
		url := ch.payment.PaymentURL(sess.UserUsername, 50)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"requiresPayment": true,
			"paymentUrl":      url,
			"amount":          50,
		})
		return
	}

	clan, err := ch.clanStore.CreateClan(body.Name, body.Tag, sess.UserSub, sess.UserName)
	if err != nil {
		clanError(w, 400, err.Error())
		return
	}

	// Update user profile with clan ID
	ch.userStore.SetClanID(sess.UserSub, clan.ID)

	go ch.clanStore.Save()
	go ch.userStore.Save()

	json.NewEncoder(w).Encode(map[string]interface{}{"clan": clan})
}

// HandleCreateClanConfirm is called after payment to confirm clan creation.
// POST /api/clans/create-confirm  { "name": "...", "tag": "..." }
func (ch *ClanHandler) HandleCreateClanConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	if existing := ch.clanStore.FindClanByMember(sess.UserSub); existing != nil {
		clanError(w, 400, "you are already in a clan")
		return
	}

	var body struct {
		Name string `json:"name"`
		Tag  string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		clanError(w, 400, "invalid request body")
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	body.Tag = strings.TrimSpace(body.Tag)
	if body.Name == "" || utf8.RuneCountInString(body.Name) > 32 {
		clanError(w, 400, "clan name must be 1-32 characters")
		return
	}
	if body.Tag == "" || utf8.RuneCountInString(body.Tag) > 6 {
		clanError(w, 400, "clan tag must be 1-6 characters")
		return
	}

	// Verify the payment was received (check recent transactions for 50 beans from this user)
	if ch.payment != nil {
		txs, err := ch.payment.GetTransactions()
		if err != nil {
			clanError(w, 500, "failed to verify payment")
			return
		}
		found := false
		cutoff := time.Now().Add(-1 * time.Hour) // payment within the last hour
		for _, tx := range txs {
			if tx.FromUser == sess.UserUsername && tx.Amount >= 50 {
				txTime, _ := time.Parse(time.RFC3339, tx.Timestamp)
				if txTime.After(cutoff) {
					found = true
					break
				}
			}
		}
		if !found {
			clanError(w, 402, "payment not found — please pay 50 beans first")
			return
		}
	}

	clan, err := ch.clanStore.CreateClan(body.Name, body.Tag, sess.UserSub, sess.UserName)
	if err != nil {
		clanError(w, 400, err.Error())
		return
	}

	ch.userStore.SetClanID(sess.UserSub, clan.ID)
	go ch.clanStore.Save()
	go ch.userStore.Save()

	json.NewEncoder(w).Encode(map[string]interface{}{"clan": clan})
}

// HandleJoinRequest sends a join request to a clan.
// POST /api/clans/join  { "clanId": "..." }
func (ch *ClanHandler) HandleJoinRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	if existing := ch.clanStore.FindClanByMember(sess.UserSub); existing != nil {
		clanError(w, 400, "you are already in a clan")
		return
	}

	var body struct {
		ClanID string `json:"clanId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		clanError(w, 400, "invalid request body")
		return
	}

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.clans[body.ClanID]
	if clan == nil {
		clanError(w, 404, "clan not found")
		return
	}

	if !clan.Settings.AcceptingRequests {
		clanError(w, 400, "this clan is not accepting join requests")
		return
	}

	// Check min level
	user := ch.userStore.GetUser(sess.UserSub)
	if user != nil && clan.Settings.MinLevel > 1 {
		level := LevelFromPoints(user.Points)
		if level < clan.Settings.MinLevel {
			clanError(w, 400, fmt.Sprintf("you need level %d to join this clan (you are level %d)", clan.Settings.MinLevel, level))
			return
		}
	}

	if len(clan.Members) >= clan.Settings.MaxMembers {
		clanError(w, 400, "this clan is full")
		return
	}

	// Check if already requested
	if clan.FindRequest(sess.UserSub) >= 0 {
		clanError(w, 400, "you already have a pending request")
		return
	}

	clan.JoinRequests = append(clan.JoinRequests, ClanJoinRequest{
		Sub:         sess.UserSub,
		Name:        sess.UserName,
		RequestedAt: time.Now().Unix(),
	})

	go ch.clanStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "requested"})
}

// HandleAcceptRequest accepts a join request. Requires Elder+ role.
// POST /api/clans/accept  { "sub": "..." }
func (ch *ClanHandler) HandleAcceptRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		clanError(w, 400, "invalid request body")
		return
	}

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	if me == nil || ClanRoleLevel(me.Role) < ClanRoleLevel(ClanRoleElder) {
		clanError(w, 403, "insufficient permissions (requires Elder+)")
		return
	}

	idx := clan.FindRequest(body.Sub)
	if idx < 0 {
		clanError(w, 404, "no pending request from this user")
		return
	}

	req := clan.JoinRequests[idx]
	// Remove request
	clan.JoinRequests = append(clan.JoinRequests[:idx], clan.JoinRequests[idx+1:]...)

	// Add member
	clan.Members = append(clan.Members, ClanMember{
		Sub:      req.Sub,
		Name:     req.Name,
		Role:     ClanRoleMember,
		JoinedAt: time.Now().Unix(),
	})

	ch.userStore.SetClanID(req.Sub, clan.ID)
	go ch.clanStore.Save()
	go ch.userStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// HandleRejectRequest rejects a join request. Requires Elder+ role.
// POST /api/clans/reject  { "sub": "..." }
func (ch *ClanHandler) HandleRejectRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Sub string `json:"sub"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	if me == nil || ClanRoleLevel(me.Role) < ClanRoleLevel(ClanRoleElder) {
		clanError(w, 403, "insufficient permissions (requires Elder+)")
		return
	}

	idx := clan.FindRequest(body.Sub)
	if idx < 0 {
		clanError(w, 404, "no pending request from this user")
		return
	}

	clan.JoinRequests = append(clan.JoinRequests[:idx], clan.JoinRequests[idx+1:]...)
	go ch.clanStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "rejected"})
}

// HandleKickMember removes a member from the clan. Requires Elder+ role.
// Cannot kick members of equal or higher role.
// POST /api/clans/kick  { "sub": "..." }
func (ch *ClanHandler) HandleKickMember(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Sub string `json:"sub"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	target := clan.FindMember(body.Sub)
	if me == nil || target == nil {
		clanError(w, 404, "member not found")
		return
	}

	if ClanRoleLevel(me.Role) < ClanRoleLevel(ClanRoleElder) {
		clanError(w, 403, "insufficient permissions (requires Elder+)")
		return
	}

	if ClanRoleLevel(target.Role) >= ClanRoleLevel(me.Role) {
		clanError(w, 403, "cannot kick a member of equal or higher role")
		return
	}

	// Remove member
	for i, m := range clan.Members {
		if m.Sub == body.Sub {
			clan.Members = append(clan.Members[:i], clan.Members[i+1:]...)
			break
		}
	}

	ch.userStore.SetClanID(body.Sub, "")
	go ch.clanStore.Save()
	go ch.userStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "kicked"})
}

// HandleSetRole changes a member's role. Requires Leader or Co-Leader.
// Cannot promote someone to a role equal or above your own.
// POST /api/clans/set-role  { "sub": "...", "role": "member|elder|co-leader" }
func (ch *ClanHandler) HandleSetRole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Sub  string   `json:"sub"`
		Role ClanRole `json:"role"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	// Validate role
	switch body.Role {
	case ClanRoleMember, ClanRoleElder, ClanRoleCoLeader:
		// ok
	default:
		clanError(w, 400, "invalid role (must be member, elder, or co-leader)")
		return
	}

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	target := clan.FindMember(body.Sub)
	if me == nil || target == nil {
		clanError(w, 404, "member not found")
		return
	}

	if ClanRoleLevel(me.Role) < ClanRoleLevel(ClanRoleCoLeader) {
		clanError(w, 403, "requires Leader or Co-Leader")
		return
	}

	if ClanRoleLevel(body.Role) >= ClanRoleLevel(me.Role) {
		clanError(w, 403, "cannot promote to a role equal or above your own")
		return
	}

	if me.Sub == target.Sub {
		clanError(w, 400, "cannot change your own role")
		return
	}

	target.Role = body.Role
	go ch.clanStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// HandleUpdateSettings updates clan settings. Requires Leader or Co-Leader.
// POST /api/clans/settings  { settings fields... }
func (ch *ClanHandler) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Name              *string `json:"name,omitempty"`
		AcceptingRequests *bool   `json:"acceptingRequests,omitempty"`
		MinLevel          *int    `json:"minLevel,omitempty"`
		Description       *string `json:"description,omitempty"`
		ClanColor         *string `json:"clanColor,omitempty"`
		IsPublic          *bool   `json:"isPublic,omitempty"`
		MaxMembers        *int    `json:"maxMembers,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	if me == nil || ClanRoleLevel(me.Role) < ClanRoleLevel(ClanRoleCoLeader) {
		clanError(w, 403, "requires Leader or Co-Leader")
		return
	}

	// Apply updates
	if body.Name != nil {
		n := strings.TrimSpace(*body.Name)
		if n != "" && utf8.RuneCountInString(n) <= 32 {
			clan.Name = n
		}
	}
	if body.AcceptingRequests != nil {
		clan.Settings.AcceptingRequests = *body.AcceptingRequests
	}
	if body.MinLevel != nil && *body.MinLevel >= 1 && *body.MinLevel <= 100 {
		clan.Settings.MinLevel = *body.MinLevel
	}
	if body.Description != nil {
		d := strings.TrimSpace(*body.Description)
		if utf8.RuneCountInString(d) <= 200 {
			clan.Settings.Description = d
		}
	}
	if body.ClanColor != nil {
		c := strings.TrimSpace(*body.ClanColor)
		if len(c) == 7 && c[0] == '#' {
			clan.Settings.ClanColor = c
		}
	}
	if body.IsPublic != nil {
		clan.Settings.IsPublic = *body.IsPublic
	}
	if body.MaxMembers != nil && *body.MaxMembers >= 2 && *body.MaxMembers <= 200 {
		clan.Settings.MaxMembers = *body.MaxMembers
	}

	go ch.clanStore.Save()

	json.NewEncoder(w).Encode(map[string]interface{}{"clan": clan})
}

// HandleLeaveClan lets a user leave their clan.
// POST /api/clans/leave
func (ch *ClanHandler) HandleLeaveClan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	ch.clanStore.mu.Lock()
	defer ch.clanStore.mu.Unlock()

	clan := ch.clanStore.findClanByMemberLocked(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	me := clan.FindMember(sess.UserSub)
	if me == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	// Leader cannot leave without transferring leadership first
	if me.Role == ClanRoleLeader {
		// If they're the only member, disband
		if len(clan.Members) == 1 {
			delete(ch.clanStore.clans, clan.ID)
			ch.userStore.SetClanID(sess.UserSub, "")
			go ch.clanStore.Save()
			go ch.userStore.Save()
			json.NewEncoder(w).Encode(map[string]string{"status": "disbanded"})
			return
		}
		clanError(w, 400, "leader must transfer leadership before leaving (promote a co-leader first)")
		return
	}

	// Remove member
	for i, m := range clan.Members {
		if m.Sub == sess.UserSub {
			clan.Members = append(clan.Members[:i], clan.Members[i+1:]...)
			break
		}
	}

	ch.userStore.SetClanID(sess.UserSub, "")
	go ch.clanStore.Save()
	go ch.userStore.Save()

	json.NewEncoder(w).Encode(map[string]string{"status": "left"})
}

// HandleClanChat handles clan chat messages.
// POST /api/clans/chat  { "text": "..." }
func (ch *ClanHandler) HandleClanChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		clanError(w, 405, "method not allowed")
		return
	}
	sess := ch.getSession(r)
	if sess == nil {
		clanError(w, 401, "not authenticated")
		return
	}

	var body struct {
		Text string `json:"text"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	text := strings.TrimSpace(body.Text)
	if text == "" || utf8.RuneCountInString(text) > 200 {
		clanError(w, 400, "message must be 1-200 characters")
		return
	}

	clan := ch.clanStore.FindClanByMember(sess.UserSub)
	if clan == nil {
		clanError(w, 400, "you are not in a clan")
		return
	}

	// Broadcast clan chat to all clan members via the callback
	if ch.clanChatFn != nil {
		ch.clanChatFn(clan.ID, sess.UserName, sess.UserSub, text)
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}
