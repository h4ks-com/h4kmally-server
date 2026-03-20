package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ShopItem defines a purchasable item in the token shop.
type ShopItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Price        int    `json:"price"`                  // cost in beans
	Tokens       int    `json:"tokens"`                 // tokens awarded (skin or effect or total for bundles)
	Type         string `json:"type"`                   // "skin", "effect", or "bundle"
	SkinTokens   int    `json:"skinTokens,omitempty"`   // for bundles: skin tokens
	EffectTokens int    `json:"effectTokens,omitempty"` // for bundles: effect tokens
	Section      string `json:"section"`                // "skin", "effect", or "bundle" — display grouping
}

// ShopOrder tracks a pending or completed purchase.
type ShopOrder struct {
	ID           string `json:"id"`
	UserSub      string `json:"userSub"`
	Username     string `json:"username"` // beans bank username (for tx matching)
	ItemID       string `json:"itemId"`
	Amount       int    `json:"amount"`                 // beans to pay
	Tokens       int    `json:"tokens"`                 // total tokens to award
	TokenType    string `json:"tokenType"`              // "skin", "effect", or "bundle"
	SkinTokens   int    `json:"skinTokens,omitempty"`   // for bundles
	EffectTokens int    `json:"effectTokens,omitempty"` // for bundles
	Status       string `json:"status"`                 // "pending", "completed", "expired"
	CreatedAt    int64  `json:"createdAt"`              // unix timestamp
	MatchedTx    int    `json:"matchedTx,omitempty"`    // matched transaction ID
}

// DailyGiftState tracks a user's daily gift.
type DailyGiftState struct {
	Code      string `json:"code"`
	RedeemURL string `json:"redeemUrl"`
	Amount    int    `json:"amount"`
	CreatedAt int64  `json:"createdAt"`
	Redeemed  bool   `json:"redeemed"`
}

// ShopHandler manages the token shop and daily gift system.
type ShopHandler struct {
	authMgr   *AuthManager
	userStore *UserStore
	payment   PaymentProvider

	mu     sync.Mutex
	orders []*ShopOrder

	// Set of processed transaction IDs (to avoid double-fulfillment)
	processedTxs map[int]bool

	items []ShopItem
}

// NewShopHandler creates a new shop handler.
func NewShopHandler(authMgr *AuthManager, userStore *UserStore, payment PaymentProvider) *ShopHandler {
	sh := &ShopHandler{
		authMgr:      authMgr,
		userStore:    userStore,
		payment:      payment,
		orders:       make([]*ShopOrder, 0),
		processedTxs: make(map[int]bool),
		items: []ShopItem{
			// ── Skin Tokens (base: 1.0 token/bean) ──
			{ID: "skin-5", Name: "5 Skin Tokens", Price: 5, Tokens: 5, Type: "skin", Section: "skin"},
			{ID: "skin-15", Name: "15 Skin Tokens", Price: 12, Tokens: 15, Type: "skin", Section: "skin"},
			{ID: "skin-35", Name: "35 Skin Tokens", Price: 25, Tokens: 35, Type: "skin", Section: "skin"},
			// ── Effect Tokens (premium: ~0.6-0.8 token/bean) ──
			{ID: "effect-3", Name: "3 Effect Tokens", Price: 5, Tokens: 3, Type: "effect", Section: "effect"},
			{ID: "effect-8", Name: "8 Effect Tokens", Price: 12, Tokens: 8, Type: "effect", Section: "effect"},
			{ID: "effect-20", Name: "20 Effect Tokens", Price: 25, Tokens: 20, Type: "effect", Section: "effect"},
			// ── Bundles (best deal: 33-53% savings vs individual) ──
			{ID: "bundle-starter", Name: "Starter Pack", Price: 8, Tokens: 13, Type: "bundle", SkinTokens: 8, EffectTokens: 5, Section: "bundle"},
			{ID: "bundle-pro", Name: "Pro Pack", Price: 18, Tokens: 32, Type: "bundle", SkinTokens: 20, EffectTokens: 12, Section: "bundle"},
			{ID: "bundle-ultimate", Name: "Ultimate Pack", Price: 30, Tokens: 70, Type: "bundle", SkinTokens: 45, EffectTokens: 25, Section: "bundle"},
		},
	}

	// Load persisted orders
	sh.loadOrders()

	// Start background order fulfillment poller
	go sh.pollTransactions()

	return sh
}

func generateOrderID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleShopItems returns the list of available shop items.
// GET /api/shop/items
func (sh *ShopHandler) HandleShopItems(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":    sh.items,
		"currency": sh.payment.Name(),
	})
}

// HandleDailyGift manages the daily gift system.
// GET /api/shop/daily-gift?session=TOKEN
// Returns the current daily gift (creating one if needed).
func (sh *ShopHandler) HandleDailyGift(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	session := sh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	user := sh.userStore.Get(session.UserSub)
	if user == nil {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now().Unix()

	// Check if user has an existing daily gift from the last 24h
	if user.DailyGiftCode != "" && now-user.LastDailyGift < 86400 {
		// Gift exists and is within 24h — check if it was redeemed
		gift, err := sh.payment.GetGiftLink(user.DailyGiftCode)
		if err == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"gift": DailyGiftState{
					Code:      gift.Code,
					RedeemURL: gift.RedeemURL,
					Amount:    gift.Amount,
					CreatedAt: user.LastDailyGift,
					Redeemed:  gift.Redeemed,
				},
				"available": false,
			})
			return
		}
		// If we can't fetch it, treat as expired and allow new one
	}

	// Check if 24h has passed since last gift
	if user.LastDailyGift > 0 && now-user.LastDailyGift < 86400 {
		// Already claimed today but code is gone — just return status
		json.NewEncoder(w).Encode(map[string]interface{}{
			"gift":       nil,
			"available":  false,
			"nextGiftAt": user.LastDailyGift + 86400,
		})
		return
	}

	// Create a new daily gift
	gift, err := sh.payment.CreateGiftLink(3, "24h", "Daily h4kmally gift! 🎮")
	if err != nil {
		log.Printf("[Shop] Failed to create daily gift for %s: %v", session.UserName, err)
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"failed to create gift"}`))
		return
	}

	// Update user profile
	sh.userStore.SetDailyGift(session.UserSub, gift.Code, now)

	log.Printf("[Shop] Created daily gift for %s: code=%s amount=%d", session.UserName, gift.Code, gift.Amount)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"gift": DailyGiftState{
			Code:      gift.Code,
			RedeemURL: gift.RedeemURL,
			Amount:    gift.Amount,
			CreatedAt: now,
			Redeemed:  false,
		},
		"available": true,
	})
}

// HandlePurchase initiates a shop purchase.
// POST /api/shop/purchase?session=TOKEN  body: { "itemId": "tokens-3" }
func (sh *ShopHandler) HandlePurchase(w http.ResponseWriter, r *http.Request) {
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

	session := sh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		ItemID string `json:"itemId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}

	// Find the item
	var item *ShopItem
	for i := range sh.items {
		if sh.items[i].ID == body.ItemID {
			item = &sh.items[i]
			break
		}
	}
	if item == nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid item"}`))
		return
	}

	// Use the Logto username as the beans bank username (same identity provider)
	username := session.UserUsername
	if username == "" {
		username = session.UserName // fallback to display name
	}
	if username == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"username required for payment"}`))
		return
	}

	sh.mu.Lock()

	// Check for existing pending order for same item from same user
	for _, o := range sh.orders {
		if o.UserSub == session.UserSub && o.ItemID == item.ID && o.Status == "pending" {
			// Return existing pending order
			payURL := sh.payment.PaymentURL(o.Username, o.Amount)
			sh.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"order":      o,
				"paymentUrl": payURL,
			})
			return
		}
	}

	// Create new order
	order := &ShopOrder{
		ID:           generateOrderID(),
		UserSub:      session.UserSub,
		Username:     username,
		ItemID:       item.ID,
		Amount:       item.Price,
		Tokens:       item.Tokens,
		TokenType:    item.Type,
		SkinTokens:   item.SkinTokens,
		EffectTokens: item.EffectTokens,
		Status:       "pending",
		CreatedAt:    time.Now().Unix(),
	}
	sh.orders = append(sh.orders, order)
	sh.saveOrders()
	sh.mu.Unlock()

	payURL := sh.payment.PaymentURL(username, item.Price)

	log.Printf("[Shop] New order %s: user=%s item=%s amount=%d tokens=%d",
		order.ID, username, item.ID, item.Price, item.Tokens)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"order":      order,
		"paymentUrl": payURL,
	})
}

// HandleOrders returns the user's order history.
// GET /api/shop/orders?session=TOKEN
func (sh *ShopHandler) HandleOrders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	session := sh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	userOrders := make([]*ShopOrder, 0)
	for _, o := range sh.orders {
		if o.UserSub == session.UserSub {
			userOrders = append(userOrders, o)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"orders": userOrders,
	})
}

// HandleCancelOrder lets a user cancel their own pending order.
// POST /api/shop/cancel?session=TOKEN  body: {"orderId":"..."}
func (sh *ShopHandler) HandleCancelOrder(w http.ResponseWriter, r *http.Request) {
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

	session := sh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		OrderID string `json:"orderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OrderID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"orderId is required"}`))
		return
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	var target *ShopOrder
	for _, o := range sh.orders {
		if o.ID == body.OrderID {
			target = o
			break
		}
	}

	if target == nil {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"order not found"}`))
		return
	}

	if target.UserSub != session.UserSub {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"not your order"}`))
		return
	}

	if target.Status != "pending" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "only pending orders can be cancelled"})
		return
	}

	target.Status = "cancelled"
	sh.saveOrders()

	log.Printf("[Shop] Order %s cancelled by user %s", target.ID, session.UserUsername)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"order": target,
	})
}

// pollTransactions runs in the background, checking for incoming payments.
func (sh *ShopHandler) pollTransactions() {
	// Initial delay
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		sh.checkTransactions()
		sh.expireOldOrders()
	}
}

func (sh *ShopHandler) checkTransactions() {
	txns, err := sh.payment.GetTransactions()
	if err != nil {
		// Don't spam logs — only log occasionally
		return
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	for _, tx := range txns {
		// Skip already processed transactions
		if sh.processedTxs[tx.ID] {
			continue
		}

		// Look for a matching pending order
		// Match: same username (case-insensitive), same amount, incoming to merchant
		for _, order := range sh.orders {
			if order.Status != "pending" {
				continue
			}
			if !strings.EqualFold(order.Username, tx.FromUser) {
				continue
			}
			if order.Amount != tx.Amount {
				continue
			}

			// Match found! Fulfill the order
			order.Status = "completed"
			order.MatchedTx = tx.ID
			sh.processedTxs[tx.ID] = true

			// Grant tokens to the user based on type
			switch order.TokenType {
			case "effect":
				sh.userStore.GrantEffectTokens(order.UserSub, order.Tokens)
			case "bundle":
				sh.userStore.GrantTokens(order.UserSub, order.SkinTokens)
				sh.userStore.GrantEffectTokens(order.UserSub, order.EffectTokens)
			default:
				sh.userStore.GrantTokens(order.UserSub, order.Tokens)
			}

			log.Printf("[Shop] Order %s fulfilled: user=%s type=%s tokens=%d txID=%d",
				order.ID, order.Username, order.TokenType, order.Tokens, tx.ID)

			sh.saveOrders()
			break // one tx per order
		}

		// Mark as processed even if no match, to avoid re-checking
		sh.processedTxs[tx.ID] = true
	}
}

func (sh *ShopHandler) expireOldOrders() {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := time.Now().Unix()
	changed := false
	for _, order := range sh.orders {
		if order.Status == "pending" && now-order.CreatedAt > 3600 {
			order.Status = "expired"
			changed = true
			log.Printf("[Shop] Order %s expired: user=%s", order.ID, order.Username)
		}
	}
	if changed {
		sh.saveOrders()
	}
}

// Persistence — simple JSON file for orders
func (sh *ShopHandler) loadOrders() {
	data, err := readJSONFile("data/shop_orders.json")
	if err != nil {
		return
	}
	var state struct {
		Orders       []*ShopOrder `json:"orders"`
		ProcessedTxs []int        `json:"processedTxs"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	sh.orders = state.Orders
	if sh.orders == nil {
		sh.orders = make([]*ShopOrder, 0)
	}
	for _, id := range state.ProcessedTxs {
		sh.processedTxs[id] = true
	}
}

func (sh *ShopHandler) saveOrders() {
	txIDs := make([]int, 0, len(sh.processedTxs))
	for id := range sh.processedTxs {
		txIDs = append(txIDs, id)
	}
	state := struct {
		Orders       []*ShopOrder `json:"orders"`
		ProcessedTxs []int        `json:"processedTxs"`
	}{
		Orders:       sh.orders,
		ProcessedTxs: txIDs,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	writeJSONFile("data/shop_orders.json", data)
}

func readJSONFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func writeJSONFile(path string, data []byte) {
	_ = os.WriteFile(path, data, 0644)
}
