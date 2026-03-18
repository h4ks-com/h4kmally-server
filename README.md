# h4kmally Server

An open-source re-implementation of an [agar.io](https://agar.io) game server in Go, inspired by the private server re-implementation (closed source) at [sigmally.com](https://sigmally.com).

Uses the **SIG 0.0.1** binary WebSocket protocol with opcode shuffling, and is designed to be paired with a compatible web client.

## Features

- Full agar.io-style gameplay: eating, splitting, ejecting mass, viruses, decay
- Ogar-faithful physics: exponential velocity decay, border bouncing, mass-based eating
- Binary WebSocket protocol with 256-byte opcode shuffle table
- OAuth2 authentication (Logto)
- Admin panel: manage users, bans, skins
- Skin system with categories (free, level-gated, premium)
- Leaderboard, spectator mode, spawn protection
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
│   │   └── users.go         # User store, points, unlocks
│   ├── game/
│   │   ├── config.go        # All tunable game parameters
│   │   ├── engine.go        # Core game simulation (tick, spawning, physics, collisions)
│   │   ├── cell.go          # Cell types, constructors, eat logic
│   │   ├── grid.go          # Spatial grid for collision detection
│   │   └── player.go        # Player state, input queues, helpers
│   └── protocol/
│       ├── opcodes.go       # SIG 0.0.1 opcode definitions, shuffle table
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
- **premium** — granted individually via token reveals

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

## Configuration

All settings can be tuned via **environment variables** or a `.env` file. Unset variables use defaults.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3001` | HTTP/WebSocket listen port |
| `LOGTO_ENDPOINT` | *(required)* | Logto OAuth2 endpoint URL |
| `SUPER_ADMIN` | | Username to auto-grant admin |
| `MAP_WIDTH` | `7071` | Half-width of the map (border: ±value) |
| `MAP_HEIGHT` | `7071` | Half-height of the map |
| `START_SIZE` | `100` | Initial cell radius on spawn |
| `MIN_PLAYER_SIZE` | `100` | Minimum cell size |
| `MIN_SPLIT_SIZE` | `122.5` | Minimum radius to split |
| `MAX_CELLS` | `16` | Maximum cells per player |
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

### Mass Formula

$$\text{mass} = \frac{\text{size}^2}{100} \qquad \text{size} = \sqrt{\text{mass} \times 100}$$

## HTTP Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/ws/` | WS | WebSocket game connection |
| `/api/auth/me` | GET | Current user info |
| `/api/auth/profile` | GET/POST | User profile |
| `/api/auth/tokens/reveal` | POST | Reveal token rewards |
| `/api/skins` | GET | Full skin manifest |
| `/api/skins/access` | GET | Access-filtered skin list |
| `/skins/*` | GET | Skin image files |
| `/api/admin/*` | Various | Admin panel endpoints |

## Protocol

See [PROTOCOL.md](PROTOCOL.md) for the complete SIG 0.0.1 binary protocol specification.

## License

MIT
