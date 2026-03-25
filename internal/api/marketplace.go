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

const (
	defaultMarketplaceMaxPrice  = 100
	marketplacePurchaseExpiryS  = 3600 // 1 hour
	marketplaceDataFile         = "data/marketplace.json"
)

// TradeItem describes one component of a trade offer or request.
type TradeItem struct {
	ItemType string `json:"itemType"` // "skin_token", "effect_token", "powerup"
	ItemKey  string `json:"itemKey"`
	ItemName string `json:"itemName"` // human-readable label
	Quantity int    `json:"quantity"`
}

// MarketplaceListing represents an item listed for sale or trade by a player.
type MarketplaceListing struct {
	ID             string `json:"id"`
	SellerSub      string `json:"sellerSub"`      // used internally; omitted from public listing feeds
	SellerUsername string `json:"sellerUsername"` // display name
	ItemType       string `json:"itemType"`       // "skin_token", "effect_token", "powerup"
	ItemKey        string `json:"itemKey"`        // skin name, effect name, or PowerupType string
	ItemName       string `json:"itemName"`       // human-readable label
	Quantity       int    `json:"quantity"`
	Price          int    `json:"price"`   // beans total for the full listing
	Status         string `json:"status"`  // "active" | "sold" | "cancelled" | "reversed"
	CreatedAt      int64  `json:"createdAt"`

	// Populated when sold
	BuyerSub      string `json:"buyerSub,omitempty"`
	BuyerUsername string `json:"buyerUsername,omitempty"`
	SoldAt        int64  `json:"soldAt,omitempty"`
	PaymentTxID   int    `json:"paymentTxId,omitempty"`
	PayoutErr     string `json:"payoutErr,omitempty"` // non-empty if seller payout failed

	// Populated when reversed (admin action)
	ReversedAt      int64  `json:"reversedAt,omitempty"`
	ReversedBy      string `json:"reversedBy,omitempty"`
	ReversalNote    string `json:"reversalNote,omitempty"`
	RefundSent      bool   `json:"refundSent,omitempty"`      // buyer refund succeeded
	RefundErr       string `json:"refundErr,omitempty"`       // non-empty if buyer refund failed
	ItemsClawedBack bool   `json:"itemsClawedBack,omitempty"` // whether buyer still had the items

	// Trade listing fields
	ListingType string      `json:"listingType"`            // "sale" (default) | "trade"
	WantedItems []TradeItem `json:"wantedItems,omitempty"`  // what seller wants in return (trade only)
}

// MarketplacePendingPurchase tracks a buyer's outstanding payment for a listing.
type MarketplacePendingPurchase struct {
	ID            string `json:"id"`
	ListingID     string `json:"listingId"`
	BuyerSub      string `json:"buyerSub"`
	BuyerUsername string `json:"buyerUsername"` // beans bank username used for tx matching
	Amount        int    `json:"amount"`
	Status        string `json:"status"` // "pending" | "completed" | "expired"
	CreatedAt     int64  `json:"createdAt"`
	PaymentTxID   int    `json:"paymentTxId,omitempty"`
}

// MarketplaceHandler manages the player-to-player marketplace.
type MarketplaceHandler struct {
	authMgr   *AuthManager
	userStore *UserStore
	payment   PaymentProvider
	maxPrice  int

	mu           sync.Mutex
	listings     []*MarketplaceListing
	pending      []*MarketplacePendingPurchase
	processedTxs map[int]bool
}

// NewMarketplaceHandler creates and starts a marketplace handler.
func NewMarketplaceHandler(authMgr *AuthManager, userStore *UserStore, payment PaymentProvider, maxPrice int) *MarketplaceHandler {
	if maxPrice <= 0 {
		maxPrice = defaultMarketplaceMaxPrice
	}
	mh := &MarketplaceHandler{
		authMgr:      authMgr,
		userStore:    userStore,
		payment:      payment,
		maxPrice:     maxPrice,
		listings:     make([]*MarketplaceListing, 0),
		pending:      make([]*MarketplacePendingPurchase, 0),
		processedTxs: make(map[int]bool),
	}
	mh.load()
	go mh.pollTransactions()
	return mh
}

// ── Public / user endpoints ───────────────────────────────────────────────────

// HandleListings returns all active listings (public — no auth required).
// GET /api/marketplace/listings
func (mh *MarketplaceHandler) HandleListings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	mh.mu.Lock()
	// Build a public-safe view (omit sellerSub/buyerSub)
	type publicListing struct {
		ID             string      `json:"id"`
		SellerUsername string      `json:"sellerUsername"`
		ItemType       string      `json:"itemType"`
		ItemKey        string      `json:"itemKey"`
		ItemName       string      `json:"itemName"`
		Quantity       int         `json:"quantity"`
		Price          int         `json:"price"`
		Status         string      `json:"status"`
		CreatedAt      int64       `json:"createdAt"`
		ListingType    string      `json:"listingType"`
		WantedItems    []TradeItem `json:"wantedItems,omitempty"`
	}
	out := make([]publicListing, 0)
	for _, l := range mh.listings {
		if l.Status == "active" {
			out = append(out, publicListing{
				ID:             l.ID,
				SellerUsername: l.SellerUsername,
				ItemType:       l.ItemType,
				ItemKey:        l.ItemKey,
				ItemName:       l.ItemName,
				Quantity:       l.Quantity,
				Price:          l.Price,
				Status:         l.Status,
				CreatedAt:      l.CreatedAt,
				ListingType:    l.ListingType,
				WantedItems:    l.WantedItems,
			})
		}
	}
	mh.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{"listings": out})
}

// HandleMyItems returns the authenticated user's marketable items.
// GET /api/marketplace/my-items?session=TOKEN
func (mh *MarketplaceHandler) HandleMyItems(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	items := mh.userStore.GetMarketableItems(session.UserSub)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":    items,
		"maxPrice": mh.maxPrice,
	})
}

// HandleMyListings returns the authenticated user's own listings (all statuses).
// GET /api/marketplace/my-listings?session=TOKEN
func (mh *MarketplaceHandler) HandleMyListings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	mh.mu.Lock()
	userListings := make([]*MarketplaceListing, 0)
	for _, l := range mh.listings {
		if l.SellerSub == session.UserSub {
			userListings = append(userListings, l)
		}
	}
	mh.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{"listings": userListings})
}

// HandleMyPurchases returns purchase history for the authenticated user.
// GET /api/marketplace/my-purchases?session=TOKEN
func (mh *MarketplaceHandler) HandleMyPurchases(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	mh.mu.Lock()
	// Listings where this user is the buyer
	bought := make([]*MarketplaceListing, 0)
	for _, l := range mh.listings {
		if l.BuyerSub == session.UserSub {
			bought = append(bought, l)
		}
	}
	// Pending purchases
	userPending := make([]*MarketplacePendingPurchase, 0)
	for _, p := range mh.pending {
		if p.BuyerSub == session.UserSub && p.Status == "pending" {
			userPending = append(userPending, p)
		}
	}
	mh.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"purchases": bought,
		"pending":   userPending,
	})
}

// HandleCreateListing creates a new listing and escrows items from the seller.
// POST /api/marketplace/list?session=TOKEN
// Body: { "itemType":"skin_token","itemKey":"dragon","quantity":3,"price":10 }
func (mh *MarketplaceHandler) HandleCreateListing(w http.ResponseWriter, r *http.Request) {
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
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		ItemType    string      `json:"itemType"`
		ItemKey     string      `json:"itemKey"`
		Quantity    int         `json:"quantity"`
		Price       int         `json:"price"`
		ListingType string      `json:"listingType"` // "sale" (default) or "trade"
		WantedItems []TradeItem `json:"wantedItems"` // trade listings only
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}

	if body.ListingType == "" {
		body.ListingType = "sale"
	}
	if body.ListingType != "sale" && body.ListingType != "trade" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"listingType must be sale or trade"}`))
		return
	}

	// Validate offered item
	if body.ItemType != "skin_token" && body.ItemType != "effect_token" && body.ItemType != "powerup" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"itemType must be skin_token, effect_token, or powerup"}`))
		return
	}
	if body.ItemKey == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"itemKey is required"}`))
		return
	}
	if body.Quantity < 1 {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"quantity must be at least 1"}`))
		return
	}

	// Sale-specific: price validation
	if body.ListingType == "sale" {
		if body.Price < 1 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"price must be at least 1 bean"}`))
			return
		}
		if body.Price > mh.maxPrice {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("price cannot exceed %d beans", mh.maxPrice),
			})
			return
		}
	}

	// Trade-specific: validate wantedItems
	if body.ListingType == "trade" {
		if len(body.WantedItems) == 0 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"trade listings must specify at least one wantedItem"}`))
			return
		}
		for i, wi := range body.WantedItems {
			if wi.ItemType != "skin_token" && wi.ItemType != "effect_token" && wi.ItemType != "powerup" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"wantedItem itemType must be skin_token, effect_token, or powerup"}`))
				return
			}
			if wi.ItemKey == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"wantedItem itemKey is required"}`))
				return
			}
			if wi.Quantity < 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"wantedItem quantity must be at least 1"}`))
				return
			}
			body.WantedItems[i].ItemName = mh.resolveItemName(wi.ItemType, wi.ItemKey)
		}
	}

	// Enforce max 1 active listing per user per item type+key
	mh.mu.Lock()
	for _, l := range mh.listings {
		if l.SellerSub == session.UserSub && l.Status == "active" &&
			l.ItemType == body.ItemType && l.ItemKey == body.ItemKey {
			mh.mu.Unlock()
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"you already have an active listing for this item"}`))
			return
		}
	}
	mh.mu.Unlock()

	// Resolve display name for the item
	itemName := mh.resolveItemName(body.ItemType, body.ItemKey)

	// Escrow: deduct items from seller's account
	ok := mh.escrowItems(session.UserSub, body.ItemType, body.ItemKey, body.Quantity)
	if !ok {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"you do not have enough of this item to list"}`))
		return
	}

	// Use session username as beans bank username (consistent with shop)
	sellerUsername := session.UserUsername
	if sellerUsername == "" {
		sellerUsername = session.UserName
	}

	listing := &MarketplaceListing{
		ID:             generateOrderID(),
		SellerSub:      session.UserSub,
		SellerUsername: sellerUsername,
		ItemType:       body.ItemType,
		ItemKey:        body.ItemKey,
		ItemName:       itemName,
		Quantity:       body.Quantity,
		Price:          body.Price,
		ListingType:    body.ListingType,
		WantedItems:    body.WantedItems,
		Status:         "active",
		CreatedAt:      time.Now().Unix(),
	}

	mh.mu.Lock()
	mh.listings = append(mh.listings, listing)
	mh.save()
	mh.mu.Unlock()

	log.Printf("[Marketplace] Listing created: id=%s seller=%s item=%s/%s qty=%d price=%d",
		listing.ID, sellerUsername, body.ItemType, body.ItemKey, body.Quantity, body.Price)

	json.NewEncoder(w).Encode(map[string]interface{}{"listing": listing})
}

// HandleCancelListing cancels an active listing and returns escrow to the seller.
// POST /api/marketplace/cancel?session=TOKEN
// Body: { "listingId": "abc123" }
func (mh *MarketplaceHandler) HandleCancelListing(w http.ResponseWriter, r *http.Request) {
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
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		ListingID string `json:"listingId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ListingID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"listingId is required"}`))
		return
	}

	mh.mu.Lock()
	var target *MarketplaceListing
	for _, l := range mh.listings {
		if l.ID == body.ListingID {
			target = l
			break
		}
	}
	if target == nil {
		mh.mu.Unlock()
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"listing not found"}`))
		return
	}
	if target.SellerSub != session.UserSub {
		mh.mu.Unlock()
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"not your listing"}`))
		return
	}
	if target.Status != "active" {
		mh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"only active listings can be cancelled"}`))
		return
	}
	// Check no pending purchase is in-flight for this listing
	for _, p := range mh.pending {
		if p.ListingID == target.ID && p.Status == "pending" {
			mh.mu.Unlock()
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"listing has a pending purchase — wait for it to expire before cancelling"}`))
			return
		}
	}
	target.Status = "cancelled"
	mh.save()
	mh.mu.Unlock()

	// Return escrowed items to seller
	mh.returnEscrow(target.SellerSub, target.ItemType, target.ItemKey, target.Quantity)

	log.Printf("[Marketplace] Listing %s cancelled by seller %s", target.ID, target.SellerUsername)

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "listing": target})
}

// HandleBuyListing initiates a purchase — returns a payment URL for the buyer.
// POST /api/marketplace/buy?session=TOKEN
// Body: { "listingId": "abc123" }
func (mh *MarketplaceHandler) HandleBuyListing(w http.ResponseWriter, r *http.Request) {
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
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		ListingID string `json:"listingId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ListingID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"listingId is required"}`))
		return
	}

	buyerUsername := session.UserUsername
	if buyerUsername == "" {
		buyerUsername = session.UserName
	}
	if buyerUsername == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"username required for payment"}`))
		return
	}

	mh.mu.Lock()

	// Find active listing
	var listing *MarketplaceListing
	for _, l := range mh.listings {
		if l.ID == body.ListingID && l.Status == "active" {
			listing = l
			break
		}
	}
	if listing == nil {
		mh.mu.Unlock()
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"listing not found or no longer available"}`))
		return
	}
	if listing.ListingType == "trade" {
		mh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"this is a trade listing — use /api/marketplace/accept-trade"}`))
		return
	}
	if listing.SellerSub == session.UserSub {
		mh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"you cannot buy your own listing"}`))
		return
	}

	// Check for existing pending purchase by this buyer for this listing
	for _, p := range mh.pending {
		if p.ListingID == listing.ID && p.BuyerSub == session.UserSub && p.Status == "pending" {
			// Return existing pending purchase
			payURL := mh.payment.PaymentURL(p.BuyerUsername, p.Amount)
			mh.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"purchase":   p,
				"paymentUrl": payURL,
				"listing":    listing,
			})
			return
		}
	}

	// Check no other buyer has an active pending purchase for this listing
	for _, p := range mh.pending {
		if p.ListingID == listing.ID && p.Status == "pending" {
			mh.mu.Unlock()
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"this listing is pending payment by another buyer — try again shortly"}`))
			return
		}
	}

	// Create new pending purchase
	purchase := &MarketplacePendingPurchase{
		ID:            generateOrderID(),
		ListingID:     listing.ID,
		BuyerSub:      session.UserSub,
		BuyerUsername: buyerUsername,
		Amount:        listing.Price,
		Status:        "pending",
		CreatedAt:     time.Now().Unix(),
	}
	mh.pending = append(mh.pending, purchase)
	mh.save()
	mh.mu.Unlock()

	payURL := mh.payment.PaymentURL(buyerUsername, listing.Price)

	log.Printf("[Marketplace] Purchase initiated: purchaseId=%s listingId=%s buyer=%s amount=%d",
		purchase.ID, listing.ID, buyerUsername, listing.Price)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"purchase":   purchase,
		"paymentUrl": payURL,
		"listing":    listing,
	})
}

// HandleAcceptTrade performs an atomic item-for-item swap for a trade listing.
// POST /api/marketplace/accept-trade?session=TOKEN
// Body: { "listingId": "abc123" }
func (mh *MarketplaceHandler) HandleAcceptTrade(w http.ResponseWriter, r *http.Request) {
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
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	var body struct {
		ListingID string `json:"listingId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ListingID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"listingId is required"}`))
		return
	}

	buyerUsername := session.UserUsername
	if buyerUsername == "" {
		buyerUsername = session.UserName
	}

	// Claim the listing atomically inside the lock
	mh.mu.Lock()
	var listing *MarketplaceListing
	for _, l := range mh.listings {
		if l.ID == body.ListingID && l.Status == "active" {
			listing = l
			break
		}
	}
	if listing == nil {
		mh.mu.Unlock()
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"listing not found or no longer available"}`))
		return
	}
	if listing.ListingType != "trade" {
		mh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"not a trade listing — use /api/marketplace/buy"}`))
		return
	}
	if listing.SellerSub == session.UserSub {
		mh.mu.Unlock()
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"you cannot accept your own trade"}`))
		return
	}

	// Optimistically mark as sold to prevent concurrent accepts
	now := time.Now().Unix()
	listing.Status = "sold"
	listing.BuyerSub = session.UserSub
	listing.BuyerUsername = buyerUsername
	listing.SoldAt = now
	snapshot := *listing
	mh.save()
	mh.mu.Unlock()

	// Attempt to deduct all wanted items from the buyer
	type deducted struct {
		ItemType string
		ItemKey  string
		Qty      int
	}
	var done []deducted
	var failedItem string
	for _, wi := range snapshot.WantedItems {
		if ok := mh.escrowItems(session.UserSub, wi.ItemType, wi.ItemKey, wi.Quantity); !ok {
			failedItem = wi.ItemName
			break
		}
		done = append(done, deducted{wi.ItemType, wi.ItemKey, wi.Quantity})
	}

	if failedItem != "" {
		// Rollback: return already-deducted items to buyer
		for _, d := range done {
			mh.returnEscrow(session.UserSub, d.ItemType, d.ItemKey, d.Qty)
		}
		// Reset listing back to active
		mh.mu.Lock()
		for _, l := range mh.listings {
			if l.ID == snapshot.ID {
				l.Status = "active"
				l.BuyerSub = ""
				l.BuyerUsername = ""
				l.SoldAt = 0
				break
			}
		}
		mh.save()
		mh.mu.Unlock()
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("you do not have enough %q to complete this trade", failedItem),
		})
		return
	}

	// Success: grant the seller's offered items to the buyer
	mh.grantItems(session.UserSub, snapshot.ItemType, snapshot.ItemKey, snapshot.Quantity)

	// Grant the buyer's contributed items to the seller
	for _, d := range done {
		mh.grantItems(snapshot.SellerSub, d.ItemType, d.ItemKey, d.Qty)
	}

	log.Printf("[Marketplace] Trade completed: listing=%s buyer=%s seller=%s offered=%s/%s×%d",
		snapshot.ID, buyerUsername, snapshot.SellerUsername,
		snapshot.ItemType, snapshot.ItemKey, snapshot.Quantity)

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "listing": &snapshot})
}

// ── Admin endpoints ───────────────────────────────────────────────────────────

// HandleAdminListings returns all marketplace listings for admin review.
// GET /api/admin/marketplace?session=TOKEN
func (mh *MarketplaceHandler) HandleAdminListings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil || !mh.authMgr.UserStore.IsAdmin(session.UserSub) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"forbidden"}`))
		return
	}

	mh.mu.Lock()
	out := struct {
		Listings []*MarketplaceListing        `json:"listings"`
		Pending  []*MarketplacePendingPurchase `json:"pending"`
	}{
		Listings: mh.listings,
		Pending:  mh.pending,
	}
	mh.mu.Unlock()

	json.NewEncoder(w).Encode(out)
}

// HandleAdminReverse reverses a completed marketplace transaction.
// POST /api/admin/marketplace/reverse?session=TOKEN
// Body: { "listingId":"abc123", "note":"fraud detected" }
//
// The reversal:
//  1. Attempts to claw back items from the buyer.
//  2. Returns items to the seller.
//  3. Refunds the buyer's beans from the vendor account via POST /transfer.
//  4. Marks the listing as "reversed".
func (mh *MarketplaceHandler) HandleAdminReverse(w http.ResponseWriter, r *http.Request) {
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
	session := mh.authMgr.ValidateSession(r.URL.Query().Get("session"))
	if session == nil || !mh.authMgr.UserStore.IsAdmin(session.UserSub) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"forbidden"}`))
		return
	}

	var body struct {
		ListingID string `json:"listingId"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ListingID == "" {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"listingId is required"}`))
		return
	}

	mh.mu.Lock()
	var target *MarketplaceListing
	for _, l := range mh.listings {
		if l.ID == body.ListingID {
			target = l
			break
		}
	}
	if target == nil {
		mh.mu.Unlock()
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"listing not found"}`))
		return
	}
	if target.Status != "sold" {
		mh.mu.Unlock()
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("listing status is %q — only sold listings can be reversed", target.Status)})
		return
	}
	// Snapshot info we need before releasing the lock
	listingCopy := *target
	mh.mu.Unlock()

	now := time.Now().Unix()

	// 1. Attempt item claw-back from buyer
	clawedBack := mh.escrowItems(listingCopy.BuyerSub, listingCopy.ItemType, listingCopy.ItemKey, listingCopy.Quantity)

	// 2. Return items to seller regardless (we hold the blame for fraud, not the seller)
	mh.returnEscrow(listingCopy.SellerSub, listingCopy.ItemType, listingCopy.ItemKey, listingCopy.Quantity)

	// 3. Refund buyer from vendor account
	var refundErr string
	err := mh.payment.SendTransfer(listingCopy.BuyerUsername, listingCopy.Price)
	if err != nil {
		refundErr = err.Error()
		log.Printf("[Marketplace] REVERSAL REFUND FAILED: listing=%s buyer=%s amount=%d err=%v",
			listingCopy.ID, listingCopy.BuyerUsername, listingCopy.Price, err)
	}

	// 4. Mark listing as reversed
	mh.mu.Lock()
	for _, l := range mh.listings {
		if l.ID == listingCopy.ID {
			l.Status = "reversed"
			l.ReversedAt = now
			l.ReversedBy = session.UserName
			l.ReversalNote = body.Note
			l.RefundSent = refundErr == ""
			l.RefundErr = refundErr
			l.ItemsClawedBack = clawedBack
			break
		}
	}
	mh.save()
	mh.mu.Unlock()

	log.Printf("[Marketplace] Reversed listing %s by admin %s: clawedBack=%v refundSent=%v refundErr=%q",
		listingCopy.ID, session.UserName, clawedBack, refundErr == "", refundErr)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":             true,
		"clawedBack":     clawedBack,
		"refundSent":     refundErr == "",
		"refundErr":      refundErr,
		"listingId":      listingCopy.ID,
		"buyerRefunded":  listingCopy.Price,
		"buyerUsername":  listingCopy.BuyerUsername,
		"sellerUsername": listingCopy.SellerUsername,
	})
}

// ── Background transaction polling ───────────────────────────────────────────

func (mh *MarketplaceHandler) pollTransactions() {
	time.Sleep(7 * time.Second)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		mh.checkTransactions()
		mh.expireOldPurchases()
	}
}

func (mh *MarketplaceHandler) checkTransactions() {
	txns, err := mh.payment.GetTransactions()
	if err != nil {
		return
	}

	mh.mu.Lock()
	defer mh.mu.Unlock()

	for _, tx := range txns {
		if mh.processedTxs[tx.ID] {
			continue
		}

		// Try to match against a pending marketplace purchase
		matched := false
		for _, p := range mh.pending {
			if p.Status != "pending" {
				continue
			}
			if !strings.EqualFold(p.BuyerUsername, tx.FromUser) {
				continue
			}
			if p.Amount != tx.Amount {
				continue
			}

			// Find the listing for this purchase
			var listing *MarketplaceListing
			for _, l := range mh.listings {
				if l.ID == p.ListingID {
					listing = l
					break
				}
			}
			if listing == nil || listing.Status != "active" {
				continue
			}

			// Match found — fulfil
			p.Status = "completed"
			p.PaymentTxID = tx.ID
			mh.processedTxs[tx.ID] = true

			// Grant items to buyer (outside lock is safer, but we track inside for safety)
			buyerSub := p.BuyerSub
			buyerUsername := p.BuyerUsername
			sellerSub := listing.SellerSub
			sellerUsername := listing.SellerUsername
			itemType := listing.ItemType
			itemKey := listing.ItemKey
			quantity := listing.Quantity
			price := listing.Price

			listing.Status = "sold"
			listing.BuyerSub = buyerSub
			listing.BuyerUsername = buyerUsername
			listing.SoldAt = time.Now().Unix()
			listing.PaymentTxID = tx.ID

			mh.save()
			matched = true

			// Perform grants and payout outside the hot path in a goroutine
			// We've already marked it sold, so no double-spend risk.
			go func() {
				mh.grantItems(buyerSub, itemType, itemKey, quantity)
				// Pay the seller
				payErr := mh.payment.SendTransfer(sellerUsername, price)
				if payErr != nil {
					log.Printf("[Marketplace] PAYOUT FAILED: listing=%s seller=%s amount=%d err=%v",
						listing.ID, sellerUsername, price, payErr)
					mh.mu.Lock()
					listing.PayoutErr = payErr.Error()
					mh.save()
					mh.mu.Unlock()
				}
				log.Printf("[Marketplace] Sale completed: listing=%s buyer=%s seller=%s item=%s/%s qty=%d price=%d txID=%d",
					listing.ID, buyerUsername, sellerUsername, itemType, itemKey, quantity, price, tx.ID)
				// Suppress unused variable warning (sellerSub used for logging if needed)
				_ = sellerSub
			}()

			break
		}

		if !matched {
			mh.processedTxs[tx.ID] = true
		}
	}
}

func (mh *MarketplaceHandler) expireOldPurchases() {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	now := time.Now().Unix()
	changed := false
	for _, p := range mh.pending {
		if p.Status == "pending" && now-p.CreatedAt > marketplacePurchaseExpiryS {
			p.Status = "expired"
			changed = true
			log.Printf("[Marketplace] Purchase %s expired (listing=%s buyer=%s)", p.ID, p.ListingID, p.BuyerUsername)
		}
	}
	if changed {
		mh.save()
	}
}

// ── Escrow helpers ────────────────────────────────────────────────────────────

// escrowItems removes items from a user's account (returns false if insufficient).
func (mh *MarketplaceHandler) escrowItems(sub, itemType, itemKey string, qty int) bool {
	switch itemType {
	case "skin_token":
		return mh.userStore.DeductSkinTokens(sub, itemKey, qty)
	case "effect_token":
		return mh.userStore.DeductEffectTokens(sub, itemKey, qty)
	case "powerup":
		return mh.userStore.DeductPowerupCharges(sub, PowerupType(itemKey), qty)
	}
	return false
}

// returnEscrow grants items back to a user (used when cancelling or reversing).
func (mh *MarketplaceHandler) returnEscrow(sub, itemType, itemKey string, qty int) {
	mh.grantItems(sub, itemType, itemKey, qty)
}

// grantItems adds items to a user's account.
func (mh *MarketplaceHandler) grantItems(sub, itemType, itemKey string, qty int) {
	switch itemType {
	case "skin_token":
		mh.userStore.GrantSpecificSkinTokens(sub, itemKey, qty)
	case "effect_token":
		mh.userStore.GrantSpecificEffectTokens(sub, itemKey, qty)
	case "powerup":
		mh.userStore.GrantPowerupCharges(sub, PowerupType(itemKey), qty)
	}
}

// resolveItemName returns a human-readable name for an item type+key combination.
func (mh *MarketplaceHandler) resolveItemName(itemType, itemKey string) string {
	switch itemType {
	case "skin_token":
		return itemKey + " skin token"
	case "effect_token":
		return itemKey + " effect token"
	case "powerup":
		def := GetPowerupDef(PowerupType(itemKey))
		if def != nil {
			return def.Label + " powerup"
		}
		return itemKey + " powerup"
	}
	return itemKey
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (mh *MarketplaceHandler) save() {
	txIDs := make([]int, 0, len(mh.processedTxs))
	for id := range mh.processedTxs {
		txIDs = append(txIDs, id)
	}
	state := struct {
		Listings     []*MarketplaceListing        `json:"listings"`
		Pending      []*MarketplacePendingPurchase `json:"pending"`
		ProcessedTxs []int                         `json:"processedTxs"`
	}{
		Listings:     mh.listings,
		Pending:      mh.pending,
		ProcessedTxs: txIDs,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(marketplaceDataFile, data, 0644)
}

func (mh *MarketplaceHandler) load() {
	data, err := os.ReadFile(marketplaceDataFile)
	if err != nil {
		return
	}
	var state struct {
		Listings     []*MarketplaceListing        `json:"listings"`
		Pending      []*MarketplacePendingPurchase `json:"pending"`
		ProcessedTxs []int                         `json:"processedTxs"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	if state.Listings != nil {
		mh.listings = state.Listings
	}
	if state.Pending != nil {
		mh.pending = state.Pending
	}
	for _, id := range state.ProcessedTxs {
		mh.processedTxs[id] = true
	}
}
