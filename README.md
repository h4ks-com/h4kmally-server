# h4kmally Server

An open-source re-implementation of an [agar.io](https://agar.io) game server in Go, inspired by the private server re-implementation (closed source) at [sigmally.com](https://sigmally.com).

Uses the **SIG 0.0.2** binary WebSocket protocol with opcode shuffling, and is designed to be paired with a compatible web client.

## Features

- Full agar.io-style gameplay: eating, splitting, ejecting mass, viruses, decay
- Ogar-faithful physics: exponential velocity decay, border bouncing, mass-based eating
- Binary WebSocket protocol with 256-byte opcode shuffle table
- OAuth2 authentication (Logto)
- Admin panel: manage users, bans, skins
- Skin system with categories (free, level-gated, premium)
- **Border effects** — 4 free + 6 premium visual effects rendered around player cells
- **Token economy** — skin tokens and effect tokens, unlocked via shop or token reveals
- **Shop** — purchase tokens with Beans Bank currency; bundles available
- **Daily gift** — free Beans gift every 24 hours for logged-in users
- **Multibox** — control two independent players on a single connection
- **Top users API** — all-time highest scorers leaderboard
- Leaderboard, spectator mode (with follow + admin god-mode), spawn protection
- Server-side bots (configurable count)
- Configurable via environment variables
- Docker support

## Quick Start

### With Docker

```bash
docker build -t h4kmally-server .
docker run -p 3001:3001 \
  -e LOGTO_ENDPOINT=https://auth.example.com \
  -v ./skins:/app/skins \
  -v ./data:/app/data \
  h4kmally-server
```

### With Docker Compose

```yaml
services:
  server:
    build: .
    ports:
      - "3001:3001"
    environment:
      - LOGTO_ENDPOINT=https://auth.example.com
      - PORT=3001
    volumes:
      - ./skins:/app/skins
      - ./data:/app/data
```

### From Source

```bash
# Build
go build -o server-bin ./cmd/server

# Run (default port 3001)
LOGTO_ENDPOINT=https://auth.example.com ./server-bin

# Run on custom port
PORT=8080 LOGTO_ENDPOINT=https://auth.example.com ./server-bin
```

Or use the management script:

```bash
./manage.sh build     # compile the Go binary
./manage.sh start     # build (if needed) + start in background
./manage.sh stop      # stop the server
./manage.sh restart   # stop + start
./manage.sh status    # check if running
./manage.sh logs      # tail server log file
```

## Architecture

```
h4kmally-server/
├── cmd/server/
│   └── main.go              # Entry point, HTTP routing, tick loop
├── internal/
│   ├── api/
│   │   ├── handler.go       # WebSocket handler, client management, broadcast
│   │   ├── auth.go          # OAuth2 (Logto) authentication
│   │   ├── admin.go         # Admin panel endpoints
│   │   ├── users.go         # User store, points, levels, unlocks
│   │   ├── shop.go          # Shop, daily gift, order fulfillment
│   │   └── payment.go       # Payment provider interface (Beans Bank)
│   ├── game/
│   │   ├── config.go        # All tunable game parameters
│   │   ├── engine.go        # Core game simulation (tick, spawning, physics, collisions)
│   │   ├── cell.go          # Cell types, constructors, eat logic
│   │   ├── grid.go          # Spatial grid for collision detection
│   │   ├── player.go        # Player state, input queues, helpers
│   │   ├── bot.go           # AI bot behaviour
│   │   └── botmanager.go    # Bot spawning and lifecycle
│   └── protocol/
│       ├── opcodes.go       # SIG 0.0.2 opcode definitions, shuffle table
│       ├── parser.go        # Client message parsing
│       └── messages.go      # Server message builders (world update, leaderboard, etc.)
├── skins/                   # Skin images + manifest.json (admin-managed)
├── data/                    # Persistent data (users.json)
├── Dockerfile
├── manage.sh                # Bash management script
├── go.mod
└── go.sum
```

## Skins

Skins are managed entirely through the admin panel — no default skins are included. The admin uploads skin images and they are stored in the `skins/` directory with a `skins/manifest.json` file.

Skin categories:
- **free** — available to all players (including guests)
- **level** — unlocked at a specific player level
- **premium** — unlocked by collecting 5 skin tokens for that skin

## Effects

Border effects are visual overlays rendered around player cells. They are selected at spawn time and broadcast to all clients.

### Free Effects
| ID | Label | Description |
|----|-------|-------------|
| `neon` | Neon Pulse | Pulsing neon glow matching cell color |
| `prismatic` | Prismatic | Shifting rainbow border with hue rotation |
| `starfield` | Starfield | Orbiting star particles |
| `lightning` | Lightning | Crackling electric arcs |

### Premium Effects (require 5 effect tokens each)
| ID | Label | Description |
|----|-------|-------------|
| `sakura` | Sakura | Cherry blossom petals with wind drift and trails |
| `frost` | Frost | Ice crystal ring with frosty mist and snowflakes |
| `shadow_aura` | Shadow Aura | Dark smoke tendrils with pulsing dark-energy core |
| `flame` | Flame | Rising fire particles with ember trails |
| `glitch` | Glitch | Digital distortion — RGB shift, scan lines, data corruption |
| `blackhole` | Black Hole | Gravitational warping of nearby grid, cells, food, viruses, and border with spaghettification |

## Shop

The Shop allows authenticated users to purchase skin tokens and effect tokens using [Beans Bank](https://beans.h4ks.com) currency.

### Item Tiers

**Skin Tokens** (unlock premium skins):
| Tokens | Price | Rate |
|--------|-------|------|
| 5 | 5 beans | 1.0 tok/bean |
| 15 | 12 beans | 1.25 tok/bean |
| 35 | 25 beans | 1.4 tok/bean |

**Effect Tokens** (unlock premium effects):
| Tokens | Price | Rate |
|--------|-------|------|
| 3 | 5 beans | 0.6 tok/bean |
| 8 | 12 beans | 0.67 tok/bean |
| 20 | 25 beans | 0.8 tok/bean |

**Bundles** (both token types, best value):
| Name | Price | Skin Tokens | Effect Tokens | Total |
|------|-------|-------------|---------------|-------|
| Starter Pack | 8 beans | 8 | 5 | 13 |
| Pro Pack | 18 beans | 20 | 12 | 32 |
| Ultimate Pack | 30 beans | 45 | 25 | 70 |

### Daily Gift

Logged-in users can claim a free 3-bean Beans Bank gift every 24 hours via the shop.

## Game Mechanics

### Cell Types

| Type   | ID | Description |
|--------|----|-------------|
| Food   | 0  | Static pellets scattered across the map. Eaten by players for mass. |
| Eject  | 1  | Mass blobs ejected by players (W key). Can feed viruses or other players. |
| Player | 2  | Player-controlled cells. Can split, eject, eat others. |
| Virus  | 3  | Green spiky cells. Pop large players into pieces. |

### Eating Rules

- A cell can eat another if its mass is **≥ 1.3×** the target's mass (Ogar-faithful)
- Distance check: `distSq <= eater.Size² - target.Size² * 0.5`
- **Player vs Virus**: If the player is large enough, the player **pops** (splits into many pieces) — the virus is consumed
- **Virus vs Eject**: Virus absorbs the mass and grows; after 7 ejects it splits

### Physics

- **Movement**: Ogar-style exponential velocity decay (×0.89 per tick)
- **Splitting**: Constant boost speed regardless of cell size (~727 units total travel)
- **Ejecting**: Mass 13 blob, 15 mass cost, ±3° spread, spawns at cell edge + 16 padding
- **Borders**: Cells clamped with half-radius offset (can visually extend past border). Boosting cells (viruses, ejects, split) bounce/ricochet off walls.
- **Decay**: Large cells gradually shrink (configurable threshold and rate)
- **Spawn protection**: 75 ticks (~3 seconds) of immunity after spawning

### Multibox

A single connection can control two independent player entities simultaneously. Toggle with Tab key. See [PROTOCOL.md](PROTOCOL.md) for the wire format.

### Bots

The server can spawn AI bots (configured via `BOT_COUNT`). Bots chase nearby food and avoid larger players. They are excluded from the Top Users leaderboard.

## Configuration

All settings can be tuned via **environment variables** or a `.env` file. Unset variables use defaults.

### Server

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3001` | HTTP/WebSocket listen port |
| `LOGTO_ENDPOINT` | *(required)* | Logto OAuth2 endpoint URL |
| `SUPER_ADMIN` | | Username to auto-grant admin |
| `BOT_COUNT` | `0` | Number of server-side AI bots |

### Map & Gameplay

| Variable | Default | Description |
|---|---|---|
| `MAP_WIDTH` | `7071` | Half-width of the map (border: ±value) |
| `MAP_HEIGHT` | `7071` | Half-height of the map |
| `START_SIZE` | `100` | Initial cell radius on spawn |
| `MIN_PLAYER_SIZE` | `100` | Minimum cell size |
| `MIN_SPLIT_SIZE` | `122.5` | Minimum radius to split |
| `MAX_CELLS` | `16` | Maximum cells per player |
| `MOVE_SPEED` | `2.0` | Base movement speed multiplier |
| `DECAY_RATE` | `0.9998` | Per-tick size multiplier for decay |
| `DECAY_MIN_SIZE` | `100` | Minimum size before decay kicks in |
| `EJECT_SIZE` | `36.06` | Radius of ejected mass blobs |
| `FOOD_COUNT` | `2000` | Target number of food pellets |
| `FOOD_SIZE` | `17.3` | Food pellet radius |
| `FOOD_SPAWN_PER` | `10` | Max food spawned per tick |
| `VIRUS_COUNT` | `30` | Target number of viruses |
| `VIRUS_SIZE` | `141.4` | Virus radius |
| `VIRUS_MAX_SIZE` | `288.4` | Max virus size before splitting |
| `VIRUS_FEED_SIZE` | `36.06` | Size added per eject absorbed |
| `VIRUS_SPLIT` | `10` | Pieces when a player pops on a virus |

### Payment / Shop

| Variable | Default | Description |
|---|---|---|
| `BEANS_API_TOKEN` | *(empty)* | API token for Beans Bank (enables shop) |
| `BEANS_SITE_URL` | `https://beans.h4ks.com` | Beans Bank base URL |
| `BEANS_MERCHANT` | *(empty)* | Merchant username for Beans Bank transfers |

### Mass Formula

$$\text{mass} = \frac{\text{size}^2}{100} \qquad \text{size} = \sqrt{\text{mass} \times 100}$$

## HTTP Endpoints

### Game

| Endpoint | Method | Description |
|---|---|---|
| `/ws/` | WS | WebSocket game connection |

### Auth & Profile

| Endpoint | Method | Description |
|---|---|---|
| `/api/auth/me` | GET | Current user info |
| `/api/auth/profile` | GET/POST | User profile |
| `/api/auth/tokens/reveal` | POST | Reveal pending skin token rewards |
| `/api/auth/effect-tokens/reveal` | POST | Reveal pending effect token rewards |

### Skins & Effects

| Endpoint | Method | Description |
|---|---|---|
| `/api/skins` | GET | Full skin manifest |
| `/api/skins/access` | GET | Access-filtered skin list (respects user unlocks) |
| `/api/effects/access` | GET | All effects with per-user access info |
| `/skins/*` | GET | Skin image files |

### Shop

| Endpoint | Method | Description |
|---|---|---|
| `/api/shop/items` | GET | List shop items and pricing |
| `/api/shop/daily-gift` | GET | Get/create daily free gift |
| `/api/shop/purchase` | POST | Initiate a token purchase |
| `/api/shop/orders` | GET | User's order history |
| `/api/shop/cancel` | POST | Cancel a pending order |

### Leaderboard

| Endpoint | Method | Description |
|---|---|---|
| `/api/top-users` | GET | Top users by all-time points (`?limit=20`, max 100) |

### Admin

| Endpoint | Method | Description |
|---|---|---|
| `/api/admin/*` | Various | Admin panel endpoints (users, bans, skins, effects) |

## Protocol

See [PROTOCOL.md](PROTOCOL.md) for the complete SIG 0.0.2 binary protocol specification.

## License

MIT
