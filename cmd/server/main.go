package main

import (
	"fmt"
	"log"
	"net/http"
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
	engine := game.NewEngine(cfg)
	server := api.NewServer(engine)

	// Initialize auth (Logto OAuth2)
	logtoEndpoint := os.Getenv("LOGTO_ENDPOINT")
	if logtoEndpoint == "" {
		log.Fatal("LOGTO_ENDPOINT env var is required (e.g. https://auth.example.com)")
	}
	superAdmin := os.Getenv("SUPER_ADMIN")
	userStore := api.NewUserStore("data/users.json", superAdmin)
	authMgr := api.NewAuthManager(logtoEndpoint, userStore)
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

	// Skins API: manifest + access-checked list + image serving
	mux.HandleFunc("/api/skins", api.HandleSkinsList)
	mux.HandleFunc("/api/skins/access", server.HandleSkinsAccess)
	mux.Handle("/skins/", http.StripPrefix("/skins/", http.FileServer(http.Dir("skins"))))

	// CORS middleware for all routes
	handler := corsMiddleware(mux)

	// Start game tick loop
	go func() {
		ticker := time.NewTicker(cfg.TickRate)
		defer ticker.Stop()

		lastLB := time.Now()
		lastSave := time.Now()

		for range ticker.C {
			updated, eaten, removed := engine.Tick()
			server.Broadcast(updated, eaten, removed)

			// Broadcast leaderboard periodically
			if time.Since(lastLB) >= cfg.LeaderboardRate {
				server.BroadcastLeaderboard(engine.Leaderboard)
				lastLB = time.Now()
			}

			// Persist user data periodically (every 5 seconds)
			if time.Since(lastSave) >= 5*time.Second {
				server.SaveUserPoints()
				lastSave = time.Now()
			}
		}
	}()

	addr := ":3001"
	if envPort := os.Getenv("PORT"); envPort != "" {
		addr = ":" + envPort
	}
	fmt.Println("=== h4kmally Server (SIG 0.0.1) ===")
	fmt.Printf("  WebSocket : ws://localhost%s/ws/\n", addr)
	fmt.Printf("  HTTP      : http://localhost%s\n", addr)
	fmt.Println("  Map       : 14142 x 14142")
	fmt.Printf("  Tick Rate : %v\n", cfg.TickRate)
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
