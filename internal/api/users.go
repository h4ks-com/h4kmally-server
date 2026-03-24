package api

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/h4ks-com/h4kmally-server/internal/game"
)

// TokensPerSkinUnlock is how many tokens are needed to unlock a premium skin.
const TokensPerSkinUnlock = 5

// TokensPerEffectUnlock is how many tokens are needed to unlock a premium effect.
const TokensPerEffectUnlock = 5

// xpPerLevel is the base XP cost per level increment (cumulative quadratic).
// Total XP for level N = 50000 * N*(N-1)/2
// Level 2 = 50,000 total XP   (~1 hour of play)
// Level 3 = 150,000 total XP  (~2-3 hours)
// Level 5 = 500,000 total XP  (~8+ hours)
// Level 10 = 2,250,000 total XP
const xpPerLevelBase float64 = 50000

// LevelFromPoints calculates the player level from total points using a quadratic curve.
// Solves: points = 50000 * L*(L-1)/2  →  L = (1 + sqrt(1 + 4*points/25000)) / 2
func LevelFromPoints(points int64) int {
	if points <= 0 {
		return 1
	}
	p := float64(points)
	// Quadratic formula: L*(L-1) = 2*p/50000 = p/25000
	// L^2 - L - p/25000 = 0
	// L = (1 + sqrt(1 + 4*p/25000)) / 2
	lvl := (1.0 + math.Sqrt(1.0+4.0*p/25000.0)) / 2.0
	level := int(lvl)
	if level < 1 {
		return 1
	}
	return level
}

// XPForLevel returns the cumulative XP needed to reach a given level.
func XPForLevel(level int) int64 {
	if level <= 1 {
		return 0
	}
	l := int64(level)
	return int64(xpPerLevelBase) * l * (l - 1) / 2
}

// SkinTokenReward represents a pending token that the user hasn't revealed yet.
type SkinTokenReward struct {
	SkinName string `json:"skinName"`
}

// EffectTokenReward represents a pending effect token that the user hasn't revealed yet.
type EffectTokenReward struct {
	EffectName string `json:"effectName"`
}

// UserProfile stores persistent game stats for an authenticated user.
type UserProfile struct {
	Sub         string `json:"sub"`
	Name        string `json:"name"`
	Picture     string `json:"picture"`
	Points      int64  `json:"points"`
	GamesPlayed int64  `json:"gamesPlayed"`
	TopScore    int64  `json:"topScore"`
	IsAdmin     bool   `json:"isAdmin,omitempty"`
	BannedUntil int64  `json:"bannedUntil,omitempty"` // unix timestamp, 0 = not banned
	BanReason   string `json:"banReason,omitempty"`

	// Skin system
	SkinTokens    map[string]int    `json:"skinTokens,omitempty"`    // tokens per premium skin name
	UnlockedSkins []string          `json:"unlockedSkins,omitempty"` // premium skins unlocked (5 tokens collected)
	PendingTokens []SkinTokenReward `json:"pendingTokens,omitempty"` // tokens waiting to be revealed by user

	// Effect system
	EffectTokens        map[string]int      `json:"effectTokens,omitempty"`        // tokens per premium effect name
	UnlockedEffects     []string            `json:"unlockedEffects,omitempty"`     // premium effects unlocked (5 tokens collected)
	PendingEffectTokens []EffectTokenReward `json:"pendingEffectTokens,omitempty"` // effect tokens waiting to be revealed

	// Daily gift tracking
	LastDailyGift int64  `json:"lastDailyGift,omitempty"` // unix timestamp of last gift
	DailyGiftCode string `json:"dailyGiftCode,omitempty"` // current gift link code

	// Clan
	ClanID string `json:"clanId,omitempty"` // clan this user belongs to

	// Custom skins uploaded by this user
	CustomSkins       []string `json:"customSkins,omitempty"`
	CustomSkinSlots   int      `json:"customSkinSlots,omitempty"`   // unused upload slots purchased
	ClanCreationSlots int      `json:"clanCreationSlots,omitempty"` // unused clan creation slots purchased

	// Daily goals & powerups
	DailyState *UserDailyState `json:"dailyState,omitempty"`
}

// IsBanned returns true if the user is currently banned.
func (u *UserProfile) IsBanned() bool {
	if u.BannedUntil == 0 {
		return false
	}
	if u.BannedUntil == -1 {
		return true // permanent
	}
	return time.Now().Unix() < u.BannedUntil
}

// IPBan represents a banned IP address.
type IPBan struct {
	IP        string `json:"ip"`
	Reason    string `json:"reason"`
	BannedBy  string `json:"bannedBy"`
	ExpiresAt int64  `json:"expiresAt"` // unix timestamp, -1 = permanent
}

// IsActive returns true if the IP ban is currently in effect.
func (b *IPBan) IsActive() bool {
	if b.ExpiresAt == -1 {
		return true
	}
	return time.Now().Unix() < b.ExpiresAt
}

// StoreData is the top-level JSON structure persisted to disk.
type StoreData struct {
	Users  map[string]*UserProfile `json:"users"`
	IPBans []IPBan                 `json:"ipBans,omitempty"`
}

// UserStore manages user profiles in a JSON file.
type UserStore struct {
	mu       sync.RWMutex
	users    map[string]*UserProfile
	ipBans   []IPBan
	path     string
	superSub string // sub of the super admin (set from env)

	// PremiumSkinNames returns the list of skin names in the "premium" category.
	// Set by main after server init so token granting can pick random skins.
	PremiumSkinNames func() []string

	// PremiumEffectNames returns the list of premium effect IDs.
	// Set by main after server init so token granting can pick random effects.
	PremiumEffectNames func() []string
}

// NewUserStore creates a user store backed by the given file path.
func NewUserStore(path string, superAdminUsername string) *UserStore {
	us := &UserStore{
		users:              make(map[string]*UserProfile),
		path:               path,
		superSub:           superAdminUsername,
		PremiumSkinNames:   func() []string { return nil },
		PremiumEffectNames: func() []string { return nil },
	}
	us.load()
	return us
}

// SetSuperAdmin sets the super admin username (Logto username).
// This is matched against user names at login time.
func (us *UserStore) SetSuperAdmin(username string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	us.superSub = username
}

func (us *UserStore) load() {
	data, err := os.ReadFile(us.path)
	if err != nil {
		return // File doesn't exist yet, start fresh
	}
	// Try new format first
	var store StoreData
	if err := json.Unmarshal(data, &store); err == nil && store.Users != nil {
		us.users = store.Users
		us.ipBans = store.IPBans
		return
	}
	// Fall back to old format (just a map of users)
	_ = json.Unmarshal(data, &us.users)
}

func (us *UserStore) save() {
	store := StoreData{
		Users:  us.users,
		IPBans: us.ipBans,
	}
	data, _ := json.MarshalIndent(store, "", "  ")
	_ = os.WriteFile(us.path, data, 0644)
}

// GetOrCreate returns the user profile for the given sub, creating one if needed.
// It updates name and picture from the latest identity provider data.
func (us *UserStore) GetOrCreate(sub, name, picture string) *UserProfile {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		user = &UserProfile{
			Sub:          sub,
			Name:         name,
			Picture:      picture,
			SkinTokens:   make(map[string]int),
			EffectTokens: make(map[string]int),
		}
		us.users[sub] = user

		// Grant 5 random premium skin tokens for new accounts
		us.grantRandomTokensLocked(user, 5)
		// Grant 5 random premium effect tokens for new accounts
		us.grantRandomEffectTokensLocked(user, 5)
	} else {
		// Update name/picture from latest Logto info
		if name != "" {
			user.Name = name
		}
		if picture != "" {
			user.Picture = picture
		}
		// Ensure SkinTokens map exists for legacy accounts
		if user.SkinTokens == nil {
			user.SkinTokens = make(map[string]int)
		}
		// Ensure EffectTokens map exists for legacy accounts
		if user.EffectTokens == nil {
			user.EffectTokens = make(map[string]int)
		}
	}

	// Check if this user should be super admin (case-insensitive match by name)
	if us.superSub != "" && strings.EqualFold(name, us.superSub) {
		user.IsAdmin = true
	}

	us.save()
	return user
}

// Get returns the user profile for the given sub, or nil if not found.
func (us *UserStore) Get(sub string) *UserProfile {
	us.mu.RLock()
	defer us.mu.RUnlock()
	return us.users[sub]
}

// GetAll returns all user profiles.
func (us *UserStore) GetAll() []*UserProfile {
	us.mu.RLock()
	defer us.mu.RUnlock()
	result := make([]*UserProfile, 0, len(us.users))
	for _, u := range us.users {
		result = append(result, u)
	}
	return result
}

// GetPointsRank returns the 1-based rank of the user by all-time points.
// Returns 0 if user not found.
func (us *UserStore) GetPointsRank(sub string) int {
	us.mu.RLock()
	defer us.mu.RUnlock()
	user, exists := us.users[sub]
	if !exists || user.Points <= 0 {
		return 0
	}
	rank := 1
	for _, u := range us.users {
		if u.Sub != sub && u.Points > user.Points && !u.IsBanned() {
			rank++
		}
	}
	return rank
}

// AddScore records a game result for the user.
// Kept for backward compatibility but points are now banked live via UpdatePoints.
func (us *UserStore) AddScore(sub string, score int64) {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.GamesPlayed++
	if score > user.TopScore {
		user.TopScore = score
	}
	us.save()
}

// RecordGame increments the games played counter (points already banked live).
func (us *UserStore) RecordGame(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.GamesPlayed++
	// Note: no us.save() here — periodic SaveAll() handles persistence.
	// save() does blocking file I/O and was called from the tick goroutine.
}

// UpdatePoints adds the score delta to the user's points and updates top score.
// Called every tick for live score tracking.
// Detects level-ups and grants 3 random premium skin tokens per level gained.
func (us *UserStore) UpdatePoints(sub string, delta int64, currentScore int64) {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return
	}
	oldLevel := LevelFromPoints(user.Points)
	user.Points += delta
	newLevel := LevelFromPoints(user.Points)
	if currentScore > user.TopScore {
		user.TopScore = currentScore
	}

	// Grant 3 skin tokens per level gained (effect tokens are purchase-only)
	levelsGained := newLevel - oldLevel
	if levelsGained > 0 {
		if user.SkinTokens == nil {
			user.SkinTokens = make(map[string]int)
		}
		us.grantRandomTokensLocked(user, levelsGained*3)
	}
}

// SaveAll persists the current state to disk (called periodically).
func (us *UserStore) SaveAll() {
	us.mu.Lock()
	defer us.mu.Unlock()
	us.save()
}

// Save is an alias for SaveAll (used by external callers like clan handler).
func (us *UserStore) Save() {
	us.SaveAll()
}

// SetClanID sets the clan ID for a user.
func (us *UserStore) SetClanID(sub, clanID string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user := us.users[sub]
	if user != nil {
		user.ClanID = clanID
		us.save()
	}
}

// GetOrCreateDailyState ensures a user has a valid daily state for today.
// If the state is stale (different day), regenerate goals.
func (us *UserStore) GetOrCreateDailyState(sub string, cfg game.Config, botCount int) *UserDailyState {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return nil
	}
	today := todayKey()
	regenerate := user.DailyState == nil || user.DailyState.DateKey != today
	// Also regenerate if any goal has a zero target (bugged generation)
	if !regenerate && user.DailyState != nil {
		for _, g := range user.DailyState.Goals {
			if g.Target <= 0 {
				regenerate = true
				break
			}
		}
	}
	if regenerate {
		// Preserve existing powerups when regenerating goals
		var oldPowerups map[PowerupType]int
		if user.DailyState != nil {
			oldPowerups = user.DailyState.Powerups
		}
		goals := GenerateDailyGoals(sub, cfg, botCount)
		user.DailyState = &UserDailyState{
			DateKey:  today,
			Goals:    goals,
			Powerups: oldPowerups,
		}
	}
	return user.DailyState
}

// FlushDailyGoalProgress reads session stats from a Player and updates daily goal progress.
// Called on player death.
func (us *UserStore) FlushDailyGoalProgress(sub string, p *game.Player) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists || user.DailyState == nil {
		return
	}
	today := todayKey()
	if user.DailyState.DateKey != today {
		return // stale state, don't update
	}

	for i := range user.DailyState.Goals {
		g := &user.DailyState.Goals[i]
		if g.Completed {
			continue
		}
		switch g.Type {
		case GoalScore:
			// Cumulative mass across all lives today
			val := int(p.SessionPeakMass)
			g.Progress += val
		case GoalPlayerKills:
			g.Progress += p.SessionKills
		case GoalVirusShoot:
			g.Progress += p.SessionVirusHits
		case GoalGamesPlayed:
			g.Progress += 1
		case GoalPacifist:
			// Best no-split life peak mass
			if p.SessionNoSplit {
				val := int(p.SessionPeakMass)
				if val > g.Progress {
					g.Progress = val
				}
			}
		case GoalRevenge:
			g.Progress += p.SessionRevenge
		case GoalMassEjected:
			g.Progress += int(p.SessionMassEject)
		}
		if g.Progress >= g.Target {
			g.Progress = g.Target
			g.Completed = true
		}
	}

	// Check if all 3 goals are complete → grant powerup
	allDone := true
	for _, g := range user.DailyState.Goals {
		if !g.Completed {
			allDone = false
			break
		}
	}
	if allDone && !user.DailyState.PowerupGranted {
		user.DailyState.PowerupGranted = true
		pType, charges := GrantRandomPowerup(sub)
		if user.DailyState.Powerups == nil {
			user.DailyState.Powerups = make(map[PowerupType]int)
		}
		user.DailyState.Powerups[pType] += charges
	}
}

// LoadPowerups copies all powerups from the user's daily state into the player's inventory.
// Returns true if any powerups were loaded.
func (us *UserStore) LoadPowerups(sub string, p *game.Player) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists || user.DailyState == nil {
		return false
	}
	ds := user.DailyState
	if len(ds.Powerups) == 0 {
		return false
	}
	if p.PowerupInventory == nil {
		p.PowerupInventory = make(map[string]int)
	}
	for t, c := range ds.Powerups {
		if c > 0 {
			p.PowerupInventory[string(t)] = c
		}
	}
	return len(p.PowerupInventory) > 0
}

// DecrementPowerupCharge decreases the persistent charge count for a specific powerup type.
func (us *UserStore) DecrementPowerupCharge(sub string, pType string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists || user.DailyState == nil || user.DailyState.Powerups == nil {
		return
	}
	pt := PowerupType(pType)
	if user.DailyState.Powerups[pt] > 0 {
		user.DailyState.Powerups[pt]--
	}
	if user.DailyState.Powerups[pt] <= 0 {
		delete(user.DailyState.Powerups, pt)
	}
}

// AdminGrantPowerup adds charges of a specific powerup to a user's inventory (stacks with existing).
func (us *UserStore) AdminGrantPowerup(sub string, pType PowerupType, charges int) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return false
	}
	if user.DailyState == nil {
		user.DailyState = &UserDailyState{DateKey: todayKey()}
	}
	if user.DailyState.Powerups == nil {
		user.DailyState.Powerups = make(map[PowerupType]int)
	}
	user.DailyState.Powerups[pType] += charges
	user.DailyState.PowerupGranted = true
	return true
}

// GetUser returns a user profile by sub (alias for Get).
func (us *UserStore) GetUser(sub string) *UserProfile {
	return us.Get(sub)
}

// SetAdmin grants or revokes admin status for a user.
func (us *UserStore) SetAdmin(sub string, isAdmin bool) bool {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return false
	}
	user.IsAdmin = isAdmin
	us.save()
	return true
}

// IsAdmin checks if a user sub is an admin.
func (us *UserStore) IsAdmin(sub string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()

	user, exists := us.users[sub]
	if !exists {
		return false
	}
	return user.IsAdmin
}

// IsSuperAdmin checks if a user is the super admin.
func (us *UserStore) IsSuperAdmin(sub string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()

	user, exists := us.users[sub]
	if !exists {
		return false
	}
	return user.IsAdmin && us.superSub != "" && user.Name == us.superSub
}

// BanUser bans a user account.
func (us *UserStore) BanUser(sub string, duration int64, reason string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return false
	}
	user.BannedUntil = duration
	user.BanReason = reason
	us.save()
	return true
}

// UnbanUser removes user ban.
func (us *UserStore) UnbanUser(sub string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()

	user, exists := us.users[sub]
	if !exists {
		return false
	}
	user.BannedUntil = 0
	user.BanReason = ""
	us.save()
	return true
}

// BanIP adds an IP ban.
func (us *UserStore) BanIP(ip, reason, bannedBy string, expiresAt int64) {
	us.mu.Lock()
	defer us.mu.Unlock()

	// Remove existing ban for this IP if any
	for i, b := range us.ipBans {
		if b.IP == ip {
			us.ipBans = append(us.ipBans[:i], us.ipBans[i+1:]...)
			break
		}
	}

	us.ipBans = append(us.ipBans, IPBan{
		IP:        ip,
		Reason:    reason,
		BannedBy:  bannedBy,
		ExpiresAt: expiresAt,
	})
	us.save()
}

// UnbanIP removes an IP ban.
func (us *UserStore) UnbanIP(ip string) {
	us.mu.Lock()
	defer us.mu.Unlock()

	for i, b := range us.ipBans {
		if b.IP == ip {
			us.ipBans = append(us.ipBans[:i], us.ipBans[i+1:]...)
			us.save()
			return
		}
	}
}

// IsIPBanned checks if an IP address is banned.
func (us *UserStore) IsIPBanned(ip string) (bool, string) {
	us.mu.RLock()
	defer us.mu.RUnlock()

	for _, b := range us.ipBans {
		if b.IP == ip && b.IsActive() {
			return true, b.Reason
		}
	}
	return false, ""
}

// GetIPBans returns all IP bans.
func (us *UserStore) GetIPBans() []IPBan {
	us.mu.RLock()
	defer us.mu.RUnlock()

	result := make([]IPBan, len(us.ipBans))
	copy(result, us.ipBans)
	return result
}

// grantRandomTokensLocked picks random premium skins and adds tokens + pending reveals.
// Must be called with us.mu held.
func (us *UserStore) grantRandomTokensLocked(user *UserProfile, count int) {
	premiumSkins := us.PremiumSkinNames()
	if len(premiumSkins) == 0 {
		return
	}
	if user.SkinTokens == nil {
		user.SkinTokens = make(map[string]int)
	}
	for i := 0; i < count; i++ {
		skinName := premiumSkins[rand.Intn(len(premiumSkins))]
		user.SkinTokens[skinName]++
		user.PendingTokens = append(user.PendingTokens, SkinTokenReward{SkinName: skinName})

		// Auto-unlock when reaching the threshold
		if user.SkinTokens[skinName] >= TokensPerSkinUnlock {
			if !skinInSlice(user.UnlockedSkins, skinName) {
				user.UnlockedSkins = append(user.UnlockedSkins, skinName)
			}
		}
	}
}

// SetDailyGift updates the user's daily gift tracking.
func (us *UserStore) SetDailyGift(sub, code string, timestamp int64) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.DailyGiftCode = code
	user.LastDailyGift = timestamp
	us.save()
}

// GrantTokens grants random premium skin tokens to a user (from shop purchase).
func (us *UserStore) GrantTokens(sub string, count int) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	if user.SkinTokens == nil {
		user.SkinTokens = make(map[string]int)
	}
	us.grantRandomTokensLocked(user, count)
	us.save()
}

// RevealTokens clears the pending token list for a user.
func (us *UserStore) RevealTokens(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.PendingTokens = nil
	us.save()
}

// HasSkinUnlocked checks if a user has unlocked a specific premium skin.
func (us *UserStore) HasSkinUnlocked(sub, skinName string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()
	user, exists := us.users[sub]
	if !exists {
		return false
	}
	return skinInSlice(user.UnlockedSkins, skinName)
}

// grantRandomEffectTokensLocked picks random premium effects and adds tokens + pending reveals.
// Must be called with us.mu held.
func (us *UserStore) grantRandomEffectTokensLocked(user *UserProfile, count int) {
	premiumEffects := us.PremiumEffectNames()
	if len(premiumEffects) == 0 {
		return
	}
	if user.EffectTokens == nil {
		user.EffectTokens = make(map[string]int)
	}
	for i := 0; i < count; i++ {
		effectName := premiumEffects[rand.Intn(len(premiumEffects))]
		user.EffectTokens[effectName]++
		user.PendingEffectTokens = append(user.PendingEffectTokens, EffectTokenReward{EffectName: effectName})

		// Auto-unlock when reaching the threshold
		if user.EffectTokens[effectName] >= TokensPerEffectUnlock {
			if !stringInSlice(user.UnlockedEffects, effectName) {
				user.UnlockedEffects = append(user.UnlockedEffects, effectName)
			}
		}
	}
}

// GrantEffectTokens grants random premium effect tokens to a user (from shop purchase).
func (us *UserStore) GrantEffectTokens(sub string, count int) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	if user.EffectTokens == nil {
		user.EffectTokens = make(map[string]int)
	}
	us.grantRandomEffectTokensLocked(user, count)
	us.save()
}

// RevealEffectTokens clears the pending effect token list for a user.
func (us *UserStore) RevealEffectTokens(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.PendingEffectTokens = nil
	us.save()
}

// HasEffectUnlocked checks if a user has unlocked a specific premium effect.
func (us *UserStore) HasEffectUnlocked(sub, effectName string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()
	user, exists := us.users[sub]
	if !exists {
		return false
	}
	return stringInSlice(user.UnlockedEffects, effectName)
}

// GrantPowerupPack adds default charges for all 6 powerups to the user's daily state.
func (us *UserStore) GrantPowerupPack(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	if user.DailyState == nil {
		user.DailyState = &UserDailyState{}
	}
	if user.DailyState.Powerups == nil {
		user.DailyState.Powerups = make(map[PowerupType]int)
	}
	for _, def := range PowerupDefs {
		user.DailyState.Powerups[def.Type] += def.Charges
	}
	us.save()
}

// AddCustomSkin records a custom skin in the user's profile.
func (us *UserStore) AddCustomSkin(sub, skinName string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	if !skinInSlice(user.CustomSkins, skinName) {
		user.CustomSkins = append(user.CustomSkins, skinName)
	}
	us.save()
}

// RemoveCustomSkin removes a custom skin from the user's profile and restores their upload slot.
func (us *UserStore) RemoveCustomSkin(sub, skinName string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	for i, s := range user.CustomSkins {
		if s == skinName {
			user.CustomSkins = append(user.CustomSkins[:i], user.CustomSkins[i+1:]...)
			user.CustomSkinSlots++ // restore the slot
			break
		}
	}
	us.save()
}

// AddCustomSkinSlot increments the user's available custom skin upload slots.
func (us *UserStore) AddCustomSkinSlot(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.CustomSkinSlots++
	us.save()
}

// UseCustomSkinSlot decrements a custom skin upload slot. Returns false if none available.
func (us *UserStore) UseCustomSkinSlot(sub string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return false
	}
	if user.CustomSkinSlots <= 0 {
		return false
	}
	user.CustomSkinSlots--
	us.save()
	return true
}

// GetCustomSkinSlots returns the number of unused custom skin upload slots.
func (us *UserStore) GetCustomSkinSlots(sub string) int {
	us.mu.RLock()
	defer us.mu.RUnlock()
	user, exists := us.users[sub]
	if !exists {
		return 0
	}
	return user.CustomSkinSlots
}

// AddClanCreationSlot increments the user's available clan creation slots.
func (us *UserStore) AddClanCreationSlot(sub string) {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return
	}
	user.ClanCreationSlots++
	us.save()
}

// UseClanCreationSlot decrements a clan creation slot. Returns false if none available.
func (us *UserStore) UseClanCreationSlot(sub string) bool {
	us.mu.Lock()
	defer us.mu.Unlock()
	user, exists := us.users[sub]
	if !exists {
		return false
	}
	if user.ClanCreationSlots <= 0 {
		return false
	}
	user.ClanCreationSlots--
	us.save()
	return true
}

// GetClanCreationSlots returns the number of unused clan creation slots.
func (us *UserStore) GetClanCreationSlots(sub string) int {
	us.mu.RLock()
	defer us.mu.RUnlock()
	user, exists := us.users[sub]
	if !exists {
		return 0
	}
	return user.ClanCreationSlots
}

func skinInSlice(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func stringInSlice(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
