package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Bounty System ─────────────────────────────────────────────────────────────
//
// Players can place bounties on other players' heads.  When the target is
// killed, the killer receives the reward.  Rewards can be beans or powerups.
// Beans are escrowed from the poster via the payment provider; powerups are
// deducted from the poster's inventory upfront.

const (
	bountyFile        = "data/bounties.json"
	maxBountiesPerUser = 3 // max active bounties a single user can have
	minBeansBounty     = 10
)

// BountyRewardType describes what kind of reward a bounty pays out.
type BountyRewardType string

const (
	BountyRewardBeans   BountyRewardType = "beans"
	BountyRewardPowerup BountyRewardType = "powerup"
)

// Bounty represents an active bounty on a player.
type Bounty struct {
	ID          string           `json:"id"`
	PosterSub   string           `json:"posterSub"`
	PosterName  string           `json:"posterName"`
	TargetName  string           `json:"targetName"`  // case-insensitive match
	RewardType  BountyRewardType `json:"rewardType"`
	RewardKey   string           `json:"rewardKey,omitempty"`   // powerup type (e.g. "speed_boost") — blank for beans
	RewardAmount int             `json:"rewardAmount"`
	Status      string           `json:"status"` // "active", "claimed", "cancelled"
	ClaimedBy   string           `json:"claimedBy,omitempty"`   // name of the killer who claimed it
	ClaimedSub  string           `json:"claimedSub,omitempty"`  // sub of the killer
	CreatedAt   int64            `json:"createdAt"`
	ClaimedAt   int64            `json:"claimedAt,omitempty"`
}

// BountyHandler manages the bounty system.
type BountyHandler struct {
	mu        sync.Mutex
	bounties  []*Bounty
	authMgr   *AuthManager
	userStore *UserStore
	payment   PaymentProvider // nil if no payment provider
}

// NewBountyHandler creates a new bounty handler and loads persisted bounties.
func NewBountyHandler(authMgr *AuthManager, userStore *UserStore, payment PaymentProvider) *BountyHandler {
	bh := &BountyHandler{
		authMgr:   authMgr,
		userStore: userStore,
		payment:   payment,
	}
	bh.load()
	return bh
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (bh *BountyHandler) load() {
	data, err := os.ReadFile(bountyFile)
	if err != nil {
		bh.bounties = make([]*Bounty, 0)
		return
	}
	var bounties []*Bounty
	if err := json.Unmarshal(data, &bounties); err != nil {
		log.Printf("[Bounty] Failed to parse %s: %v", bountyFile, err)
		bh.bounties = make([]*Bounty, 0)
		return
	}
	bh.bounties = bounties
	log.Printf("[Bounty] Loaded %d bounties", len(bounties))
}

func (bh *BountyHandler) save() {
	data, err := json.MarshalIndent(bh.bounties, "", "  ")
	if err != nil {
		log.Printf("[Bounty] Failed to marshal bounties: %v", err)
		return
	}
	os.WriteFile(bountyFile, data, 0644)
}

// ── Kill Hook ─────────────────────────────────────────────────────────────────

// OnPlayerKilled is called by the server when a player is killed.
// victimName/victimSub identify the dead player.
// killerName/killerSub identify who killed them.
// Returns a list of bounties that were claimed (for notification purposes).
func (bh *BountyHandler) OnPlayerKilled(victimName, victimSub, killerName, killerSub string) []*Bounty {
	if killerName == "" {
		return nil // died to virus/edge/etc — no bounty claim
	}

	bh.mu.Lock()
	defer bh.mu.Unlock()

	var claimed []*Bounty
	now := time.Now().Unix()

	for _, b := range bh.bounties {
		if b.Status != "active" {
			continue
		}
		if !strings.EqualFold(b.TargetName, victimName) {
			continue
		}
		// Don't let the poster claim their own bounty
		if b.PosterSub == killerSub && killerSub != "" {
			continue
		}

		// Claim this bounty
		b.Status = "claimed"
		b.ClaimedBy = killerName
		b.ClaimedSub = killerSub
		b.ClaimedAt = now

		// Deliver reward
		go bh.deliverReward(b, killerName, killerSub)

		claimed = append(claimed, b)
	}

	if len(claimed) > 0 {
		bh.save()
	}

	return claimed
}

// deliverReward sends the bounty reward to the killer.
func (bh *BountyHandler) deliverReward(b *Bounty, killerName, killerSub string) {
	switch b.RewardType {
	case BountyRewardBeans:
		if bh.payment == nil {
			log.Printf("[Bounty] Cannot deliver beans reward — no payment provider")
			return
		}
		if killerName == "" {
			log.Printf("[Bounty] Cannot deliver beans reward — killer has no username")
			return
		}
		// Transfer beans from merchant to killer (beans were escrowed when bounty was placed).
		// Skip if killer IS the merchant (self-transfer would fail; merchant already holds the beans).
		if bh.payment.IsMerchant(killerName) {
			log.Printf("[Bounty] Beans reward skipped (killer is merchant): bounty=%s amount=%d",
				b.ID, b.RewardAmount)
		} else if err := bh.payment.SendTransfer(killerName, b.RewardAmount); err != nil {
			log.Printf("[Bounty] BEANS DELIVERY FAILED: bounty=%s killer=%s amount=%d err=%v",
				b.ID, killerName, b.RewardAmount, err)
		} else {
			log.Printf("[Bounty] Beans reward delivered: bounty=%s killer=%s amount=%d",
				b.ID, killerName, b.RewardAmount)
		}

	case BountyRewardPowerup:
		if killerSub == "" {
			log.Printf("[Bounty] Cannot deliver powerup reward — killer is a guest")
			return
		}
		bh.userStore.GrantPowerupCharges(killerSub, PowerupType(b.RewardKey), b.RewardAmount)
		log.Printf("[Bounty] Powerup reward delivered: bounty=%s killer=%s type=%s charges=%d",
			b.ID, killerName, b.RewardKey, b.RewardAmount)
	}
}

// ── API Handlers ──────────────────────────────────────────────────────────────

// HandleListBounties returns all active bounties.
// GET /api/bounties
func (bh *BountyHandler) HandleListBounties(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}

	bh.mu.Lock()
	active := make([]*Bounty, 0)
	for _, b := range bh.bounties {
		if b.Status == "active" {
			active = append(active, b)
		}
	}
	bh.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"bounties": active,
	})
}

// HandleMyBounties returns bounties created by the authenticated user.
// GET /api/bounties/my?session=TOKEN
func (bh *BountyHandler) HandleMyBounties(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}
	session := bh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	bh.mu.Lock()
	mine := make([]*Bounty, 0)
	for _, b := range bh.bounties {
		if b.PosterSub == session.UserSub {
			mine = append(mine, b)
		}
	}
	bh.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"bounties": mine,
	})
}

// HandleCreateBounty creates a new bounty on a player.
// POST /api/bounties/create?session=TOKEN
// Body: { "targetName": "SomePlayer", "rewardType": "beans"|"powerup", "rewardKey": "speed_boost", "rewardAmount": 50 }
func (bh *BountyHandler) HandleCreateBounty(w http.ResponseWriter, r *http.Request) {
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
	session := bh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		TargetName   string           `json:"targetName"`
		RewardType   BountyRewardType `json:"rewardType"`
		RewardKey    string           `json:"rewardKey"`
		RewardAmount int              `json:"rewardAmount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}

	body.TargetName = strings.TrimSpace(body.TargetName)
	if body.TargetName == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"targetName is required"}`))
		return
	}
	if body.RewardAmount < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"rewardAmount must be at least 1"}`))
		return
	}

	// Can't place a bounty on yourself
	posterName := session.UserUsername
	if posterName == "" {
		posterName = session.UserName
	}
	if strings.EqualFold(body.TargetName, posterName) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"you cannot place a bounty on yourself"}`))
		return
	}

	// Validate reward type and deduct cost
	switch body.RewardType {
	case BountyRewardBeans:
		if bh.payment == nil {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"beans payments are not available"}`))
			return
		}
		if body.RewardAmount < minBeansBounty {
			w.WriteHeader(400)
			w.Write([]byte(fmt.Sprintf(`{"error":"minimum beans bounty is %d"}`, minBeansBounty)))
			return
		}
		if posterName == "" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"username required for beans bounty"}`))
			return
		}

	case BountyRewardPowerup:
		// Validate powerup type
		if GetPowerupDef(PowerupType(body.RewardKey)) == nil {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid powerup type"}`))
			return
		}
		// Deduct powerup charges from poster
		if !bh.userStore.DeductPowerupCharges(session.UserSub, PowerupType(body.RewardKey), body.RewardAmount) {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"you don't have enough charges of this powerup"}`))
			return
		}

	default:
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"rewardType must be 'beans' or 'powerup'"}`))
		return
	}

	// Check max active bounties per user
	bh.mu.Lock()
	activeCount := 0
	for _, b := range bh.bounties {
		if b.PosterSub == session.UserSub && b.Status == "active" {
			activeCount++
		}
	}
	if activeCount >= maxBountiesPerUser {
		bh.mu.Unlock()
		// Refund powerup if we already deducted
		if body.RewardType == BountyRewardPowerup {
			bh.userStore.GrantPowerupCharges(session.UserSub, PowerupType(body.RewardKey), body.RewardAmount)
		}
		w.WriteHeader(400)
		w.Write([]byte(fmt.Sprintf(`{"error":"maximum %d active bounties allowed"}`, maxBountiesPerUser)))
		return
	}

	// For beans bounties, start as pending_payment until payment is confirmed.
	// For powerup bounties (already deducted), start as active.
	// Exception: if poster is the BEANS_MERCHANT, skip payment (self-transfer would fail).
	initialStatus := "active"
	if body.RewardType == BountyRewardBeans {
		if bh.payment.IsMerchant(posterName) {
			initialStatus = "active" // merchant doesn't pay themselves
		} else {
			initialStatus = "pending_payment"
		}
	}

	bounty := &Bounty{
		ID:           generateOrderID(),
		PosterSub:    session.UserSub,
		PosterName:   posterName,
		TargetName:   body.TargetName,
		RewardType:   body.RewardType,
		RewardKey:    body.RewardKey,
		RewardAmount: body.RewardAmount,
		Status:       initialStatus,
		CreatedAt:    time.Now().Unix(),
	}
	bh.bounties = append(bh.bounties, bounty)
	bh.save()
	bh.mu.Unlock()

	log.Printf("[Bounty] Created: id=%s poster=%s target=%s reward=%s/%s×%d",
		bounty.ID, posterName, body.TargetName, body.RewardType, body.RewardKey, body.RewardAmount)

	// For beans bounties, return a payment URL so the poster can pay
	// (skip for merchant — already activated above)
	var paymentUrl string
	if body.RewardType == BountyRewardBeans && !bh.payment.IsMerchant(posterName) {
		paymentUrl = bh.payment.PaymentURL(posterName, body.RewardAmount)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"bounty":     bounty,
		"paymentUrl": paymentUrl,
	})
}

// HandleCancelBounty cancels an active bounty and refunds the poster.
// POST /api/bounties/cancel?session=TOKEN
// Body: { "bountyId": "abc123" }
func (bh *BountyHandler) HandleCancelBounty(w http.ResponseWriter, r *http.Request) {
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
	session := bh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		BountyID string `json:"bountyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BountyID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"bountyId is required"}`))
		return
	}

	bh.mu.Lock()
	var target *Bounty
	for _, b := range bh.bounties {
		if b.ID == body.BountyID {
			target = b
			break
		}
	}
	if target == nil {
		bh.mu.Unlock()
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"bounty not found"}`))
		return
	}
	// Allow poster or admin to cancel
	isAdmin := false
	if user := bh.userStore.Get(session.UserSub); user != nil && user.IsAdmin {
		isAdmin = true
	}
	if target.PosterSub != session.UserSub && !isAdmin {
		bh.mu.Unlock()
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"not your bounty"}`))
		return
	}
	if target.Status != "active" {
		bh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"only active bounties can be cancelled"}`))
		return
	}
	target.Status = "cancelled"
	bh.save()
	bh.mu.Unlock()

	// Refund reward to poster
	switch target.RewardType {
	case BountyRewardPowerup:
		bh.userStore.GrantPowerupCharges(target.PosterSub, PowerupType(target.RewardKey), target.RewardAmount)
		log.Printf("[Bounty] Cancelled & refunded powerup: id=%s poster=%s type=%s charges=%d",
			target.ID, target.PosterName, target.RewardKey, target.RewardAmount)
	case BountyRewardBeans:
		// Beans were paid by the poster — refund via transfer.
		// Skip if poster is the merchant (self-transfer would fail; merchant already holds the beans).
		if bh.payment != nil {
			posterName := target.PosterName
			if bh.payment.IsMerchant(posterName) {
				log.Printf("[Bounty] Cancelled (merchant poster, no refund needed): id=%s amount=%d",
					target.ID, target.RewardAmount)
			} else if err := bh.payment.SendTransfer(posterName, target.RewardAmount); err != nil {
				log.Printf("[Bounty] BEANS REFUND FAILED: id=%s poster=%s amount=%d err=%v",
					target.ID, posterName, target.RewardAmount, err)
			} else {
				log.Printf("[Bounty] Cancelled & refunded beans: id=%s poster=%s amount=%d",
					target.ID, posterName, target.RewardAmount)
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "bounty": target})
}

// ── Beans Payment Polling ─────────────────────────────────────────────────────
// Beans bounties require the poster to pay the merchant. We poll transactions
// to confirm payment and activate the bounty. Until payment is confirmed,
// beans bounties start as "pending_payment" and become "active" once matched.

// StartPoller starts the background transaction checker for beans bounties.
func (bh *BountyHandler) StartPoller(interval time.Duration) {
	go func() {
		for {
			time.Sleep(interval)
			bh.checkBountyPayments()
		}
	}()
}

func (bh *BountyHandler) checkBountyPayments() {
	if bh.payment == nil {
		return
	}
	txns, err := bh.payment.GetTransactions()
	if err != nil {
		return
	}

	bh.mu.Lock()
	defer bh.mu.Unlock()

	for _, tx := range txns {
		for _, b := range bh.bounties {
			if b.Status != "pending_payment" {
				continue
			}
			if b.RewardType != BountyRewardBeans {
				continue
			}
			if !strings.EqualFold(b.PosterName, tx.FromUser) {
				continue
			}
			if b.RewardAmount != tx.Amount {
				continue
			}
			// Match found: activate the bounty
			b.Status = "active"
			log.Printf("[Bounty] Payment confirmed for bounty %s (poster=%s amount=%d txID=%d)",
				b.ID, b.PosterName, b.RewardAmount, tx.ID)
			break
		}
	}
	bh.save()
}
