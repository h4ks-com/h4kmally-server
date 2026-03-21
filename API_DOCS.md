# h4kmally Server — API Documentation

All HTTP endpoints are served from the game server (default port `3001`, configurable via `PORT` env var).

CORS is enabled for all routes. All JSON responses use `Content-Type: application/json`.

---

## Table of Contents

- [Public Endpoints](#public-endpoints)
  - [GET /api/status](#get-apistatus)
  - [GET /api/skins](#get-apiskins)
  - [GET /api/top-users](#get-apitop-users)
- [Chat Bridge](#chat-bridge)
  - [POST /api/chat/send](#post-apichatsend)
  - [Outgoing Webhook](#outgoing-webhook)
- [Auth Endpoints](#auth-endpoints)
  - [POST /server/recaptcha/v3](#post-serverrecaptchav3)
  - [POST /server/auth](#post-serverauth)
  - [GET /api/auth/me](#get-apiauthme)
  - [GET /api/auth/profile](#get-apiauthprofile)
  - [POST /api/auth/tokens/reveal](#post-apiauthtokensreveal)
  - [POST /api/auth/effect-tokens/reveal](#post-apiautheffect-tokensreveal)
- [Skins & Effects](#skins--effects)
  - [GET /api/skins/access](#get-apiskinsaccess)
  - [GET /api/effects/access](#get-apieffectsaccess)
  - [GET /skins/:file](#get-skinsfile)
- [Shop Endpoints](#shop-endpoints)
  - [GET /api/shop/items](#get-apishopitems)
  - [POST /api/shop/daily-gift](#post-apishopdaily-gift)
  - [POST /api/shop/purchase](#post-apishoppurchase)
  - [GET /api/shop/orders](#get-apishoporders)
  - [POST /api/shop/cancel](#post-apishopcancel)
- [Admin Endpoints](#admin-endpoints)
- [WebSocket Protocol](#websocket-protocol)
- [Environment Variables](#environment-variables)

---

## Public Endpoints

### GET /api/status

**Rate limit:** 30 requests/minute per IP.

Returns a real-time overview of the server: connected players, bots, and spectators.

**Response:**

```json
{
  "playerCount": 3,
  "botCount": 5,
  "spectatorCount": 1,
  "players": [
    {
      "name": "Alice",
      "skin": "g",
      "effect": "neon",
      "score": 4521,
      "cells": 3,
      "isBot": false,
      "clan": "ƧG"
    }
  ],
  "bots": [
    {
      "name": "phantom",
      "skin": "c",
      "effect": "flame",
      "score": 120,
      "cells": 1,
      "isBot": true
    }
  ],
  "spectators": [
    { "name": "Bob" }
  ]
}
```

### GET /api/skins

Returns the raw skins manifest JSON (the list of all available skins with name, file, category, rarity).

### GET /api/top-users

Returns the top users by score. Requires authenticated session query parameter.

---

## Chat Bridge

The chat bridge enables bidirectional communication between the game's in-game chat and external services (e.g., Discord bots, IRC bridges).

**Required env vars:**

| Variable | Description |
|---|---|
| `CHAT_BRIDGE_TOKEN` | Bearer token for authenticating incoming messages. **Required** to enable the bridge. |
| `CHAT_WEBHOOK_URL` | URL to POST outgoing chat messages to. Optional — if empty, outgoing is disabled. |

### POST /api/chat/send

Send a message into the game's chat from an external source.

**Authentication:** `Authorization: Bearer <CHAT_BRIDGE_TOKEN>`

**Rate limit:** 10 requests/minute per IP.

**Request body:**

```json
{
  "name": "DiscordBot",
  "message": "Hello from Discord!",
  "color": [100, 200, 255]
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Display name shown in chat (max 50 chars) |
| `message` | string | Yes | Chat message text (max 200 chars) |
| `color` | [r, g, b] | No | RGB color for the name. Defaults to random bright color if omitted. |

**Success response (200):**

```json
{ "ok": true }
```

**Error responses:**

| Status | Body |
|---|---|
| 401 | `{ "ok": false, "error": "invalid or missing bearer token" }` |
| 429 | `{ "ok": false, "error": "rate limit exceeded" }` |
| 400 | `{ "ok": false, "error": "name and message are required" }` |

### Outgoing Webhook

When `CHAT_WEBHOOK_URL` is set, every in-game chat message is POSTed to that URL as a JSON payload.

The request includes `Authorization: Bearer <CHAT_BRIDGE_TOKEN>` if the token is configured.

**Webhook payload:**

```json
{
  "name": "Alice",
  "message": "hello everyone!",
  "color": [200, 150, 100],
  "isSubscriber": true,
  "clan": "ƧG"
}
```

| Field | Type | Description |
|---|---|---|
| `name` | string | Player's display name |
| `message` | string | Chat message text |
| `color` | [r, g, b] | Player's cell color |
| `isSubscriber` | boolean | Whether the player is a subscriber |
| `clan` | string | Player's clan tag (empty if none) |

---

## Auth Endpoints

All auth endpoints require a `session=TOKEN` query parameter obtained from the OAuth2 login flow.

### POST /server/recaptcha/v3

Dummy captcha endpoint. Always succeeds (no real captcha needed for private servers).

### POST /server/auth

Dummy auth endpoint. Returns a session token for the WebSocket connection.

### GET /api/auth/me

Returns the current user's profile (name, level, points, etc.).

**Query:** `?session=TOKEN`

### GET /api/auth/profile

Returns extended profile info including unlock/token state.

**Query:** `?session=TOKEN`

### POST /api/auth/tokens/reveal

Reveal pending skin tokens (from level-ups or purchases).

**Query:** `?session=TOKEN`

### POST /api/auth/effect-tokens/reveal

Reveal pending effect tokens (from purchases only).

**Query:** `?session=TOKEN`

---

## Skins & Effects

### GET /api/skins/access

Returns the user's access state for all skins (owned, locked, token progress).

**Query:** `?session=TOKEN`

### GET /api/effects/access

Returns the user's access state for all effects (free/premium, token progress).

**Query:** `?session=TOKEN`

### GET /skins/:file

Static file server for skin images (e.g., `/skins/g.png`).

---

## Shop Endpoints

Only available when `BEANS_API_TOKEN` and `BEANS_MERCHANT` env vars are set.

### GET /api/shop/items

Returns the list of purchasable items (skin tokens, effect tokens, bundles).

**Query:** `?session=TOKEN`

### POST /api/shop/daily-gift

Claim the daily free gift (beans/coins).

**Query:** `?session=TOKEN`

### POST /api/shop/purchase

Purchase an item with beans.

**Query:** `?session=TOKEN`

**Body:** `{ "itemId": "skin-5" }`

### GET /api/shop/orders

List the user's pending/completed orders.

**Query:** `?session=TOKEN`

### POST /api/shop/cancel

Cancel a pending order.

**Query:** `?session=TOKEN`

**Body:** `{ "orderId": "..." }`

---

## Admin Endpoints

All admin endpoints require an authenticated session from an admin user.

| Endpoint | Method | Description |
|---|---|---|
| `/api/admin/users` | GET | List all registered users |
| `/api/admin/online` | GET | List currently connected players |
| `/api/admin/set-admin` | POST | Grant/revoke admin status |
| `/api/admin/ban-user` | POST | Ban a user by sub |
| `/api/admin/unban-user` | POST | Unban a user |
| `/api/admin/ban-ip` | POST | Ban an IP address |
| `/api/admin/unban-ip` | POST | Unban an IP |
| `/api/admin/ip-bans` | GET | List all IP bans |
| `/api/admin/skins` | GET | List all skins |
| `/api/admin/upload-skin` | POST | Upload a new skin (multipart form) |
| `/api/admin/delete-skin` | POST | Delete a skin |
| `/api/admin/set-skin-level` | POST | Set minimum level for a skin |

---

## WebSocket Protocol

Connect to `ws://HOST:PORT/ws/` for the game protocol (SIG 0.0.2).

### Client → Server Opcodes

| Opcode | Name | Payload | Description |
|---|---|---|---|
| 0 | Spawn | name + skin + effect | Request to spawn |
| 16 | Mouse | x: f32, y: f32 | Update mouse/cursor position |
| 17 | Split | (none) | Split cells |
| 21 | Eject | (none) | Eject mass |
| 22 | MultiboxToggle | (none) | Toggle multibox on/off |
| 23 | MultiboxSwitch | (none) | Switch active multibox slot |
| 24 | DirectionLock | lock: u8 | Lock (1) or unlock (0) movement direction |
| 99 | Chat | flags: u8, message: string | Send chat message |
| 190 | SpectatorCmd | cmd: u8 | Spectator command (0x01=follow) |
| 205 | Spectate | (none) | Enter spectator mode |
| 220 | CaptchaToken | token: string | Send captcha/auth token |
| 254 | Ping | (none) | Ping (latency check) |

### Server → Client Opcodes

| Opcode | Name | Description |
|---|---|---|
| 16 | WorldUpdate | Cell updates, eats, removals |
| 17 | Camera | Camera position + zoom |
| 18 | ClearAll | Clear all known cells |
| 20 | ClearMine | Clear own cells (death) |
| 22 | MultiboxState | enabled, activeSlot, multiAlive |
| 32 | AddMyCell | New own cell ID |
| 33 | AddMultiCell | New multibox cell ID |
| 49 | LeaderboardFFA | Leaderboard entries |
| 64 | Border | Map boundaries |
| 99 | Chat | Chat message from player |
| 221 | SpawnResult | Spawn accepted/rejected |
| 254 | PingReply | Pong response |

### Direction Lock (Shift Key)

Hold **Shift** to lock your movement direction. While locked:
- Your cells continue moving in the direction they were heading when you pressed Shift
- Your cursor is freed to aim eject (W/Q) and split (Space) in any direction
- Release Shift to resume normal cursor-following movement

This enables advanced play patterns like retreating while ejecting mass forward, or splitting in a different direction from your movement.

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3001` | HTTP/WS listen port |
| `MAP_WIDTH` | `7071` | Half-width of the map |
| `MAP_HEIGHT` | `7071` | Half-height of the map |
| `BOT_COUNT` | `0` | Number of AI bots to spawn |
| `LOGTO_ENDPOINT` | (required) | Logto OAuth2 endpoint URL |
| `SUPER_ADMIN` | (none) | Username to auto-grant admin |
| `CHAT_BRIDGE_TOKEN` | (none) | Bearer token for chat bridge auth. Enables `/api/chat/send`. |
| `CHAT_WEBHOOK_URL` | (none) | URL to POST outgoing chat messages to |
| `BEANS_API_TOKEN` | (none) | Payment provider API token (enables shop) |
| `BEANS_MERCHANT` | (none) | Payment provider merchant ID |
| `BEANS_SITE_URL` | `https://beans.h4ks.com` | Payment provider base URL |
| `START_SIZE` | `31.62` | Starting cell radius |
| `MIN_SPLIT_SIZE` | `59.16` | Minimum size to split |
| `MAX_CELLS` | `16` | Maximum cells per player |
| `EJECT_SIZE` | `36.06` | Ejected mass radius |
| `FOOD_COUNT` | `2000` | Target food count on map |
| `VIRUS_COUNT` | `50` | Target virus count on map |
| `MOVE_SPEED` | `1.0` | Movement speed multiplier |
| `DECAY_RATE` | `0.001` | Mass decay rate per tick |
