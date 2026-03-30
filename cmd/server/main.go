package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/h4ks-com/h4kmally-server/internal/api"
	"github.com/h4ks-com/h4kmally-server/internal/game"
)

// envFloat reads an env var as float64, returning fallback if unset/invalid.
func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("Warning: invalid %s=%q, using default %.1f", key, v, fallback)
		return fallback
	}
	return f
}

// envInt reads an env var as int, returning fallback if unset/invalid.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("Warning: invalid %s=%q, using default %d", key, v, fallback)
		return fallback
	}
	return i
}

func main() {
	cfg := game.DefaultConfig()

	// Override config from environment variables
	cfg.MapWidth = envFloat("MAP_WIDTH", cfg.MapWidth)
	cfg.MapHeight = envFloat("MAP_HEIGHT", cfg.MapHeight)

	cfg.StartSize = envFloat("START_SIZE", cfg.StartSize)
	cfg.MinPlayerSize = envFloat("MIN_PLAYER_SIZE", cfg.MinPlayerSize)
	cfg.MinSplitSize = envFloat("MIN_SPLIT_SIZE", cfg.MinSplitSize)
	cfg.MaxCells = envInt("MAX_CELLS", cfg.MaxCells)
	cfg.DecayRate = envFloat("DECAY_RATE", cfg.DecayRate)
	cfg.DecayMinSize = envFloat("DECAY_MIN_SIZE", cfg.DecayMinSize)
	cfg.EjectSize = envFloat("EJECT_SIZE", cfg.EjectSize)

	cfg.FoodCount = envInt("FOOD_COUNT", cfg.FoodCount)
	cfg.FoodSize = envFloat("FOOD_SIZE", cfg.FoodSize)
	cfg.FoodSpawnPer = envInt("FOOD_SPAWN_PER", cfg.FoodSpawnPer)

	cfg.VirusCount = envInt("VIRUS_COUNT", cfg.VirusCount)
	cfg.VirusSize = envFloat("VIRUS_SIZE", cfg.VirusSize)
	cfg.VirusMaxSize = envFloat("VIRUS_MAX_SIZE", cfg.VirusMaxSize)
	cfg.VirusFeedSize = envFloat("VIRUS_FEED_SIZE", cfg.VirusFeedSize)
	cfg.VirusSplit = envInt("VIRUS_SPLIT", cfg.VirusSplit)

	cfg.MoveSpeed = envFloat("MOVE_SPEED", cfg.MoveSpeed)

	botCount := envInt("BOT_COUNT", 0)

	engine := game.NewEngine(cfg)
	server := api.NewServer(engine)

	// Initialize Battle Royale
	br := game.NewBattleRoyale()
	br.BroadcastFn = func(msg string) {
		server.BroadcastSystemChat(msg)
	}
	server.BattleRoyale = br

	// Initialize Tank Manager
	server.TankMgr = game.NewTankManager()

	// Start server-side bots
	var botMgr *game.BotManager
	if botCount > 0 {
		botMgr = game.NewBotManager(engine, botCount, br)
	}
	server.BotMgr = botMgr

	// Initialize auth (Logto OAuth2)
	logtoEndpoint := os.Getenv("LOGTO_ENDPOINT")
	if logtoEndpoint == "" {
		log.Fatal("LOGTO_ENDPOINT env var is required (e.g. https://auth.example.com)")
	}
	superAdmin := os.Getenv("SUPER_ADMIN")
	userStore := api.NewUserStore("data/users.json", superAdmin)
	authMgr := api.NewAuthManager(logtoEndpoint, userStore, "data/sessions.json")
	server.AuthMgr = authMgr

	if superAdmin != "" {
		log.Printf("Super admin username: %s", superAdmin)
	}

	// Ensure data directory exists
	os.MkdirAll("data", 0755)

	// Admin handler
	adminHandler := api.NewAdminHandler(server, authMgr)

	// Wire up PremiumSkinNames so token granting can pick random premium skins
	userStore.PremiumSkinNames = api.GetPremiumSkinNames
	// Wire up PremiumEffectNames so token granting can pick random premium effects
	userStore.PremiumEffectNames = api.GetPremiumEffectNames

	// Payment provider + Shop + Marketplace (optional — enabled when BEANS_API_TOKEN is set)
	var shopHandler *api.ShopHandler
	var marketplaceHandler *api.MarketplaceHandler
	var bountyHandler *api.BountyHandler
	beansToken := os.Getenv("BEANS_API_TOKEN")
	beansSite := os.Getenv("BEANS_SITE_URL")
	beansMerchant := os.Getenv("BEANS_MERCHANT")
	if beansToken != "" && beansMerchant != "" {
		if beansSite == "" {
			beansSite = "https://beans.h4ks.com"
		}
		payment := api.NewBeansProvider(beansSite, beansToken, beansMerchant)
		shopHandler = api.NewShopHandler(authMgr, userStore, payment)
		marketplaceMaxPrice := envInt("MARKETPLACE_MAX_PRICE", 100)
		marketplacePayment := api.NewBeansProvider(beansSite, beansToken, beansMerchant)
		marketplaceHandler = api.NewMarketplaceHandler(authMgr, userStore, marketplacePayment, marketplaceMaxPrice)
		bountyPayment := api.NewBeansProvider(beansSite, beansToken, beansMerchant)
		bountyHandler = api.NewBountyHandler(authMgr, userStore, bountyPayment)
		bountyHandler.StartPoller(10 * time.Second)
		server.BountyMgr = bountyHandler
		log.Printf("Shop + Marketplace + Bounties enabled: payment=%s merchant=%s maxPrice=%d", payment.Name(), beansMerchant, marketplaceMaxPrice)
	} else {
		// Bounties still work with powerup-only rewards even without beans
		bountyHandler = api.NewBountyHandler(authMgr, userStore, nil)
		server.BountyMgr = bountyHandler
		log.Println("Shop + Marketplace disabled (set BEANS_API_TOKEN and BEANS_MERCHANT to enable)")
		log.Println("Bounty system enabled (powerup rewards only)")
	}

	// HTTP routes
	mux := http.NewServeMux()

	// WebSocket endpoint
	mux.HandleFunc("/ws/", server.HandleWS)

	// Dummy auth endpoints (no real captcha needed for our server)
	mux.HandleFunc("/server/recaptcha/v3", api.HandleRecaptcha)
	mux.HandleFunc("/server/auth", api.HandleAuth)

	// Auth endpoints (Logto OAuth2)
	mux.HandleFunc("/api/auth/me", authMgr.HandleAuthMe)
	mux.HandleFunc("/api/auth/profile", authMgr.HandleAuthProfile)
	mux.HandleFunc("/api/auth/tokens/reveal", authMgr.HandleTokenReveal)
	mux.HandleFunc("/api/auth/effect-tokens/reveal", authMgr.HandleEffectTokenReveal)

	// Admin endpoints
	mux.HandleFunc("/api/admin/users", adminHandler.HandleAdminUsers)
	mux.HandleFunc("/api/admin/online", adminHandler.HandleAdminOnline)
	mux.HandleFunc("/api/admin/set-admin", adminHandler.HandleAdminSetAdmin)
	mux.HandleFunc("/api/admin/ban-user", adminHandler.HandleAdminBanUser)
	mux.HandleFunc("/api/admin/unban-user", adminHandler.HandleAdminUnbanUser)
	mux.HandleFunc("/api/admin/ban-ip", adminHandler.HandleAdminBanIP)
	mux.HandleFunc("/api/admin/unban-ip", adminHandler.HandleAdminUnbanIP)
	mux.HandleFunc("/api/admin/ip-bans", adminHandler.HandleAdminIPBans)
	mux.HandleFunc("/api/admin/skins", adminHandler.HandleAdminSkins)
	mux.HandleFunc("/api/admin/upload-skin", adminHandler.HandleAdminUploadSkin)
	mux.HandleFunc("/api/admin/delete-skin", adminHandler.HandleAdminDeleteSkin)
	mux.HandleFunc("/api/admin/set-skin-level", adminHandler.HandleAdminSetSkinLevel)
	mux.HandleFunc("/api/admin/edit-skin", adminHandler.HandleAdminEditSkin)
	mux.HandleFunc("/api/admin/br/start", adminHandler.HandleAdminBRStart)
	mux.HandleFunc("/api/admin/br/stop", adminHandler.HandleAdminBRStop)
	mux.HandleFunc("/api/admin/br/status", adminHandler.HandleAdminBRStatus)
	mux.HandleFunc("/api/admin/br/auto", adminHandler.HandleAdminBRAutoConfig)
	mux.HandleFunc("/api/admin/grant-powerup", adminHandler.HandleAdminGrantPowerup)
	mux.HandleFunc("/api/admin/give-mass", adminHandler.HandleAdminGiveMass)

	// Skins API: manifest + access-checked list + image serving
	mux.HandleFunc("/api/skins", api.HandleSkinsList)
	mux.HandleFunc("/api/skins/access", server.HandleSkinsAccess)
	mux.HandleFunc("/api/skins/upload", server.HandleUploadCustomSkin)
	mux.HandleFunc("/api/effects/access", server.HandleEffectsAccess)
	mux.HandleFunc("/api/top-users", server.HandleTopUsers)
	mux.HandleFunc("/api/daily-goals", server.HandleDailyGoals)
	mux.HandleFunc("/api/daily-goals/activate-powerup", server.HandleActivatePowerup)
	mux.Handle("/skins/", http.StripPrefix("/skins/", http.FileServer(http.Dir("skins"))))

	// Public API
	mux.HandleFunc("/api/status", server.HandleStatus)

	// Clan system
	clanStore := api.NewClanStore("data/clans.json")
	var clanPayment api.PaymentProvider
	if shopHandler != nil {
		clanPayment = api.NewBeansProvider(beansSite, beansToken, beansMerchant)
	}
	clanHandler := api.NewClanHandler(authMgr, userStore, clanStore, clanPayment)
	// Wire clan chat broadcast: forward to all online clan members via WebSocket
	clanHandler.SetClanChatFn(func(clanID, senderName, senderSub, text string) {
		server.BroadcastClanChat(clanID, senderName, text)
	})
	server.ClanStore = clanStore
	mux.HandleFunc("/api/clans", clanHandler.HandleListClans)
	mux.HandleFunc("/api/clans/my", clanHandler.HandleMyClan)
	mux.HandleFunc("/api/clans/detail", clanHandler.HandleClanDetail)
	mux.HandleFunc("/api/clans/create", clanHandler.HandleCreateClan)
	mux.HandleFunc("/api/clans/create-confirm", clanHandler.HandleCreateClanConfirm)
	mux.HandleFunc("/api/clans/join", clanHandler.HandleJoinRequest)
	mux.HandleFunc("/api/clans/accept", clanHandler.HandleAcceptRequest)
	mux.HandleFunc("/api/clans/reject", clanHandler.HandleRejectRequest)
	mux.HandleFunc("/api/clans/kick", clanHandler.HandleKickMember)
	mux.HandleFunc("/api/clans/set-role", clanHandler.HandleSetRole)
	mux.HandleFunc("/api/clans/settings", clanHandler.HandleUpdateSettings)
	mux.HandleFunc("/api/clans/leave", clanHandler.HandleLeaveClan)
	mux.HandleFunc("/api/clans/chat", clanHandler.HandleClanChat)
	log.Println("Clan system enabled")

	// Chat bridge (optional — enabled when CHAT_BRIDGE_TOKEN is set)
	chatToken := os.Getenv("CHAT_BRIDGE_TOKEN")
	chatWebhookURL := os.Getenv("CHAT_WEBHOOK_URL")
	if chatToken != "" {
		bridge := api.NewChatBridge(server, chatToken, chatWebhookURL)
		server.ChatBridge = bridge
		mux.HandleFunc("/api/chat/send", bridge.HandleIncoming)
		log.Printf("Chat bridge enabled (webhook=%s)", chatWebhookURL)
	} else {
		log.Println("Chat bridge disabled (set CHAT_BRIDGE_TOKEN to enable)")
	}

	// pprof endpoints for CPU/memory profiling
	mux.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)

	// Shop endpoints (only if payment provider is configured)
	if shopHandler != nil {
		mux.HandleFunc("/api/shop/items", shopHandler.HandleShopItems)
		mux.HandleFunc("/api/shop/daily-gift", shopHandler.HandleDailyGift)
		mux.HandleFunc("/api/shop/purchase", shopHandler.HandlePurchase)
		mux.HandleFunc("/api/shop/orders", shopHandler.HandleOrders)
		mux.HandleFunc("/api/shop/cancel", shopHandler.HandleCancelOrder)
	}

	// Marketplace endpoints (only if payment provider is configured)
	if marketplaceHandler != nil {
		mux.HandleFunc("/api/marketplace/listings", marketplaceHandler.HandleListings)
		mux.HandleFunc("/api/marketplace/my-items", marketplaceHandler.HandleMyItems)
		mux.HandleFunc("/api/marketplace/my-listings", marketplaceHandler.HandleMyListings)
		mux.HandleFunc("/api/marketplace/my-purchases", marketplaceHandler.HandleMyPurchases)
		mux.HandleFunc("/api/marketplace/list", marketplaceHandler.HandleCreateListing)
		mux.HandleFunc("/api/marketplace/cancel", marketplaceHandler.HandleCancelListing)
		mux.HandleFunc("/api/marketplace/buy", marketplaceHandler.HandleBuyListing)
		mux.HandleFunc("/api/marketplace/accept-trade", marketplaceHandler.HandleAcceptTrade)
		// Admin marketplace routes
		mux.HandleFunc("/api/admin/marketplace", marketplaceHandler.HandleAdminListings)
		mux.HandleFunc("/api/admin/marketplace/reverse", marketplaceHandler.HandleAdminReverse)
	}

	// Bounty system endpoints
	if bountyHandler != nil {
		bountyHandler.SetOnlineUsersFn(server.GetOnlineAuthUsers)
		mux.HandleFunc("/api/bounties", bountyHandler.HandleListBounties)
		mux.HandleFunc("/api/bounties/my", bountyHandler.HandleMyBounties)
		mux.HandleFunc("/api/bounties/on-me", bountyHandler.HandleBountiesOnMe)
		mux.HandleFunc("/api/bounties/online-users", bountyHandler.HandleOnlineUsers)
		mux.HandleFunc("/api/bounties/create", bountyHandler.HandleCreateBounty)
		mux.HandleFunc("/api/bounties/cancel", bountyHandler.HandleCancelBounty)
		log.Println("Bounty system routes registered")
	}

	// Bot management endpoints (admin only)
	mux.HandleFunc("/api/admin/bots", server.HandleBotList)
	mux.HandleFunc("/api/admin/bots/set-count", server.HandleBotSetCount)
	mux.HandleFunc("/api/admin/bots/update", server.HandleBotUpdate)

	// Share upload proxy (avoids CORS issues with s.h4ks.com)
	mux.HandleFunc("/api/share/upload", server.HandleShareUpload)

	// CORS middleware for all routes
	handler := corsMiddleware(mux)

	// Start game tick loop
	go func() {
		ticker := time.NewTicker(cfg.TickRate)
		defer ticker.Stop()

		lastLB := time.Now()
		lastSave := time.Now()
		lastClanPos := time.Now()
		lastBRBroadcast := time.Now()
		lastInactiveCheck := time.Now()

		for range ticker.C {
			updated, eaten, removed, tickNum := engine.Tick()

			if botMgr != nil {
				botMgr.Tick()
			}

			// Battle Royale tick (every game tick)
			server.TickBattleRoyale()

			// Tank session maintenance
			server.TickTankSessions()

			server.Broadcast(updated, eaten, removed, tickNum)

			// Broadcast leaderboard periodically
			if time.Since(lastLB) >= cfg.LeaderboardRate {
				server.BroadcastLeaderboard(engine.Leaderboard)
				lastLB = time.Now()
			}

			// Broadcast clan member positions (~4 ticks = 160ms)
			if time.Since(lastClanPos) >= 160*time.Millisecond {
				server.BroadcastClanPositions()
				lastClanPos = time.Now()
			}

			// Broadcast battle royale zone updates (~5 Hz = 200ms)
			if time.Since(lastBRBroadcast) >= 200*time.Millisecond {
				server.BroadcastBattleRoyale()
				lastBRBroadcast = time.Now()
			}

			// Persist user data periodically (every 5 seconds) — in background goroutine
			// to avoid blocking the tick loop with file I/O.
			if time.Since(lastSave) >= 5*time.Second {
				go server.SaveUserPoints()
				lastSave = time.Now()
			}

			// Disconnect clients that haven't sent any message/pong in 10 seconds.
			// Prevents ghost cells from silently-dead connections.
			if time.Since(lastInactiveCheck) >= 2*time.Second {
				server.CheckInactiveClients()
				lastInactiveCheck = time.Now()
			}
		}
	}()

	addr := ":3001"
	if envPort := os.Getenv("PORT"); envPort != "" {
		addr = ":" + envPort
	}
	fmt.Println("=== h4kmally Server (SIG 0.0.2) ===")
	fmt.Printf("  WebSocket : ws://localhost%s/ws/\n", addr)
	fmt.Printf("  HTTP      : http://localhost%s\n", addr)
	fmt.Println("  Map       : 14142 x 14142")
	fmt.Printf("  Tick Rate : %v\n", cfg.TickRate)
	fmt.Printf("  Bots      : %d\n", botCount)
	fmt.Println("====================================")

	log.Fatal(http.ListenAndServe(addr, handler))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}
