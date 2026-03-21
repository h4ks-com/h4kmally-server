# SIG 0.0.2 Protocol Specification

The SIG 0.0.2 protocol is a binary WebSocket protocol used for communication between the h4kmally game client and server. All messages are sent as binary WebSocket frames. Multi-byte integers are **little-endian**.

## Table of Contents

- [Connection Handshake](#connection-handshake)
- [Opcode Shuffling](#opcode-shuffling)
- [Data Types](#data-types)
- [Client → Server Messages](#client--server-messages)
- [Server → Client Messages](#server--client-messages)
- [World Update Format](#world-update-format)
- [Border Effects (Visual)](#border-effects-visual)
- [Multibox System](#multibox-system)
- [Typical Message Flow](#typical-message-flow)

---

## Connection Handshake

### Step 1: Client sends protocol version

The client sends a UTF-8 null-terminated string:

```
"SIG 0.0.2\0"
```

### Step 2: Server responds with version + shuffle table

```
Offset  Size     Description
0       var      "SIG 0.0.2\0" — null-terminated UTF-8 string (11 bytes)
11      256      Shuffle table — a byte permutation (0–255 shuffled)
```

Total: **267 bytes**

The shuffle table is a random permutation of bytes 0–255. It is used to obfuscate opcodes on the wire (see [Opcode Shuffling](#opcode-shuffling)).

### Step 3: Server sends BORDER

Immediately after the handshake, the server sends a `BORDER` message defining the map boundaries. The message must be at least **34 bytes** (33 bytes of border data + 1 padding byte) to trigger the client's ping loop.

---

## Opcode Shuffling

All opcodes are shuffled through a per-connection permutation table to prevent trivial packet sniffing.

- **Client → Server**: The client receives the shuffle table `T[256]` and uses the **inverse** mapping. When sending logical opcode `op`, the wire byte is `T.inverse[op]`.
- **Server → Client**: The server uses `T.forward[logical_op]` to get the wire byte.

In code:
```
// Server encoding (server → client)
wire_byte = shuffle.Forward[logical_opcode]

// Server decoding (client → server)
logical_opcode = shuffle.Inverse[wire_byte]
```

The client must build the inverse table from the forward table received in the handshake.

---

## Data Types

| Notation   | Size    | Description                        |
|------------|---------|------------------------------------|
| `u8`       | 1 byte  | Unsigned 8-bit integer             |
| `u16`      | 2 bytes | Unsigned 16-bit integer (LE)       |
| `i16`      | 2 bytes | Signed 16-bit integer (LE)         |
| `u32`      | 4 bytes | Unsigned 32-bit integer (LE)       |
| `i32`      | 4 bytes | Signed 32-bit integer (LE)         |
| `f32`      | 4 bytes | IEEE 754 float (LE)                |
| `f64`      | 8 bytes | IEEE 754 double (LE)               |
| `string`   | var     | Null-terminated UTF-8 string       |
| `bool`     | 1 byte  | 0 = false, 1 = true               |

---

## Client → Server Messages

### SPAWN (opcode 0)

Requests spawning into the game.

```
Offset  Type      Field
0       u8        opcode (shuffled 0)
1       string    JSON payload (null-terminated)
```

JSON payload:
```json
{
  "name": "PlayerName",
  "skin": "skin_id",
  "effect": "neon",
  "showClanmates": false,
  "token": "auth_token",
  "email": "user@example.com"
}
```

The `effect` field selects a border effect rendered around the player's cells. See [Border Effects](#border-effects-visual) for the full list.

### MOUSE (opcode 16)

Updates the player's mouse/target position.

```
Offset  Type    Field
0       u8      opcode (shuffled 16)
1       i32     x — world X coordinate
5       i32     y — world Y coordinate
```

Total: **9 bytes**

Should be sent at ~30 Hz for smooth movement.

### SPLIT (opcode 17)

Requests splitting all eligible cells.

```
Offset  Type    Field
0       u8      opcode (shuffled 17)
```

Total: **1 byte**

### EJECT (opcode 21)

Requests ejecting mass from all cells toward the cursor.

```
Offset  Type    Field
0       u8      opcode (shuffled 21)
```

Total: **1 byte**

This shoots a small blob of mass (size 38, ~14.44 mass) from each cell toward the current mouse position. The cell must be at least size 60 to eject.

### MULTIBOX_TOGGLE (opcode 22)

Toggles multibox mode on/off. When enabled, the server creates a second player entity on the same connection. See [Multibox System](#multibox-system).

```
Offset  Type    Field
0       u8      opcode (shuffled 22)
```

Total: **1 byte**

### MULTIBOX_SWITCH (opcode 23)

Switches which multibox slot is actively receiving input (mouse, split, eject). Triggered by the Tab key.

```
Offset  Type    Field
0       u8      opcode (shuffled 23)
```

Total: **1 byte**

### DIRECTION_LOCK (opcode 24)

Locks or unlocks the player's movement direction. While locked, cells continue moving along the heading at lock time; the mouse cursor is freed for aiming eject/split independently.

```
Offset  Type    Field
0       u8      opcode (shuffled 24)
1       u8      lock (1 = lock current heading, 0 = unlock)
```

Total: **2 bytes**

Triggered by holding/releasing the **Shift** key.

### CHAT (opcode 99)

Sends a chat message.

```
Offset  Type    Field
0       u8      opcode (shuffled 99)
1       u8      flags (0 = normal)
2       string  message text (null-terminated)
```

### SPECTATOR_CMD (opcode 190)

Spectator sub-commands.

```
Offset  Type    Field
0       u8      opcode (shuffled 190)
1       u8      command
```

Total: **2 bytes**

| Command | Action | Description |
|---------|--------|-------------|
| `0x01`  | Toggle follow | Auto-follow the top player vs free-roam |
| `0x02`  | Toggle god mode | Admin-only: see entire map (no viewport culling) |

### STAT_UPDATE (opcode 191)

Keepalive acknowledgment from client.

```
Offset  Type    Field
0       u8      opcode (shuffled 191)
```

### SPECTATE (opcode 205)

Request to enter spectator mode.

```
Offset  Type    Field
0       u8      opcode (shuffled 205)
```

### CAPTCHA_TOKEN (opcode 220)

Sends authentication token.

```
Offset  Type    Field
0       u8      opcode (shuffled 220)
1       string  JSON payload: {"token": "..."}
```

### PING (opcode 254)

Latency measurement ping.

```
Offset  Type    Field
0       u8      opcode (shuffled 254)
```

Total: **1 byte**

---

## Server → Client Messages

### WORLD_UPDATE (opcode 16)

The primary game state message, sent every tick (~25 Hz). See [World Update Format](#world-update-format) for the detailed binary layout.

### CAMERA (opcode 17)

Updates the client's camera center position.

```
Offset  Type    Field
0       u8      opcode (shuffled 17)
1       f32     x — camera center X
5       f32     y — camera center Y
9       f32     zoom — (currently unused, sent as 0)
```

Total: **13 bytes**

### CLEAR_ALL (opcode 18)

Tells the client to clear all cell data.

```
Offset  Type    Field
0       u8      opcode (shuffled 18)
```

Total: **1 byte**

### CLEAR_MINE (opcode 20)

Tells the client that all their cells are gone (death).

```
Offset  Type    Field
0       u8      opcode (shuffled 20)
```

Total: **1 byte**

### MULTIBOX_STATE (opcode 22)

Notifies the client of multibox state changes.

```
Offset  Type    Field
0       u8      opcode (shuffled 22)
1       u8      enabled (1 = multibox active, 0 = off)
2       u8      activeSlot (0 = primary, 1 = secondary)
3       u8      multiAlive (1 = secondary player is alive)
```

Total: **4 bytes**

### ADD_MY_CELL (opcode 32)

Notifies the client that a cell belongs to the primary player.

```
Offset  Type    Field
0       u8      opcode (shuffled 32)
1       u32     cellId — the cell ID to claim
```

Total: **5 bytes**

### ADD_MULTI_CELL (opcode 33)

Notifies the client that a cell belongs to the secondary multibox player.

```
Offset  Type    Field
0       u8      opcode (shuffled 33)
1       u32     cellId — the cell ID to claim
```

Total: **5 bytes**

### LEADERBOARD_FFA (opcode 49)

Free-for-all leaderboard update.

```
Offset  Type    Field
0       u8      opcode (shuffled 49)
1       u32     count — number of entries

For each entry:
  +0     u32     isMe (1 if this is the current player, 0 otherwise)
  +4     string  name (null-terminated)
  +var   u32     rank (1-based)
  +var   u32     isSubscriber (0 or 1)
```

### BORDER (opcode 64)

Defines the map boundaries.

```
Offset  Type    Field
0       u8      opcode (shuffled 64)
1       f64     left   (min X)
9       f64     top    (min Y)
17      f64     right  (max X)
25      f64     bottom (max Y)
```

Total: **33 bytes** (padded to 34 for client ping-loop trigger)

### CHAT_RECV (opcode 99)

A chat message from another player.

```
Offset  Type    Field
0       u8      opcode (shuffled 99)
1       u8      flags (0 = normal)
2       u8      red   — sender name color R
3       u8      green — sender name color G
4       u8      blue  — sender name color B
5       string  sender name (null-terminated)
var     string  message text (null-terminated)
```

### SPAWN_RESULT (opcode 221)

Response to a spawn request.

```
Offset  Type    Field
0       u8      opcode (shuffled 221)
1       u8      accepted (1 = success, 0 = rejected)
```

Total: **2 bytes**

### PING_REPLY (opcode 254)

Response to client ping.

```
Offset  Type    Field
0       u8      opcode (shuffled 254)
```

Total: **1 byte**

---

## World Update Format

The `WORLD_UPDATE` message (opcode 16) is the most complex message. It's sent every server tick and contains all changes since the last tick.

### Layout

```
Section 1: Header
  Offset  Type    Field
  0       u8      opcode (shuffled 16)
  1       u16     eatCount — number of eat events

Section 2: Eat Events (repeated eatCount times)
  +0      u32     eaterCellId
  +4      u32     eatenCellId

Section 3: Cell Updates (repeated until sentinel)
  For each updated cell:
    +0      u32     cellId
    +4      i16     x — world X position
    +6      i16     y — world Y position
    +8      u16     size — cell radius
    +10     u8      flags (bitmask):
                      0x02 = has color data
                      0x04 = has skin data
                      0x08 = has name data
                      0x10 = has effect data
    +11     u8      isVirus (0 or 1)
    +12     u8      isPlayer (0 or 1)
    +13     u8      isSubscriber (0 or 1)
    +14     string  clan (null-terminated, can be empty "\0")

    If flags & 0x02 (color):
      +var    u8      red
      +var    u8      green
      +var    u8      blue

    If flags & 0x04 (skin):
      +var    string  skin URL/ID (null-terminated)

    If flags & 0x08 (name):
      +var    string  display name (null-terminated)

    If flags & 0x10 (effect):
      +var    string  effect ID (null-terminated, e.g. "neon", "blackhole")

  End sentinel:
    +0      u32     0x00000000  (cellId = 0 marks end of cell updates)

Section 4: Removals
  +0      u16     removeCount — number of cells removed
  For each removal:
    +0      u32     cellId
```

### Cell Update Flags

The flags byte controls which optional fields are present:

| Bit  | Value | Field       | When set                                    |
|------|-------|-------------|---------------------------------------------|
| 1    | 0x02  | Color       | 3 bytes (R, G, B) follow after clan         |
| 2    | 0x04  | Skin        | Null-terminated skin string follows          |
| 3    | 0x08  | Name        | Null-terminated name string follows          |
| 4    | 0x10  | Effect      | Null-terminated effect ID string follows     |

Flags are only set when the data has changed (dirty). On first appearance, all flags are typically set. On subsequent updates (position/size changes), flags may be 0 if color/skin/name/effect haven't changed.

### Cell Types in Updates

| isVirus | isPlayer | Type      | Visual                          |
|---------|----------|-----------|---------------------------------|
| 0       | 0        | Food/Eject| Small colored circle            |
| 0       | 1        | Player    | Large circle with name + outline |
| 1       | 0        | Virus     | Semi-transparent green, spiky    |

---

## Border Effects (Visual)

Players can select a visual effect rendered around their cells. The effect ID is sent in the SPAWN payload and broadcast to other clients via the `0x10` flag in world updates. Effects are cosmetic only — they have no gameplay impact (except the black hole's visual distortion of nearby entities on other clients' renderers).

### Free Effects

| ID | Label | Description |
|----|-------|-------------|
| `neon` | Neon Pulse | Pulsing neon glow matching cell color |
| `prismatic` | Prismatic | Shifting rainbow border with hue rotation |
| `starfield` | Starfield | Orbiting star particles around the cell |
| `lightning` | Lightning | Crackling electric arcs between random points |

### Premium Effects (require effect tokens)

| ID | Label | Description |
|----|-------|-------------|
| `sakura` | Sakura | Cherry blossom petals drifting with wind, leaving trails |
| `frost` | Frost | Ice crystal ring with frosty mist and snowflake particles |
| `shadow_aura` | Shadow Aura | Dark smoke tendrils radiating outward with pulsing core |
| `flame` | Flame | Rising fire particles with ember trails |
| `glitch` | Glitch | Digital distortion — RGB shift, scan lines, data corruption |
| `blackhole` | Black Hole | Gravitational warping of nearby game objects |

### Black Hole Gravitational Warping

The Black Hole effect is unique in that it distorts the rendering of other game objects on the client. The renderer:

1. Collects all black hole cells each frame
2. Warps the map grid lines toward black hole centers using inverse-distance pull with a smoothstep fade at the edge
3. Warps the positions of nearby food, viruses, and other player cells toward the black hole
4. Applies **spaghettification** — objects near a black hole are stretched radially and compressed tangentially (smaller objects like food are affected more strongly)
5. Warps border lines that pass through the gravitational field

The warp radius extends to `size × 2.5` around each black hole cell. The pull strength uses a smoothstep falloff to prevent visual discontinuity at the warp boundary edge.

---

## Multibox System

Multibox allows a single connection to control two independent player entities. This is toggled via `MULTIBOX_TOGGLE` (opcode 22).

### Behaviour

- Each connection can have at most one primary and one secondary player
- `MULTIBOX_SWITCH` (opcode 23) toggles which slot receives mouse/split/eject input
- The server sends `MULTIBOX_STATE` (opcode 22) to inform the client of the current state
- Primary cells are signaled via `ADD_MY_CELL` (opcode 32), secondary via `ADD_MULTI_CELL` (opcode 33)
- Both players auto-respawn after death while multibox is enabled
- The client renders the inactive slot's cells with reduced opacity and an active-slot ring indicator

---

## Typical Message Flow

```
Client                              Server
  |                                    |
  |--- "SIG 0.0.2\0" --------------->|  (1) Protocol version
  |                                    |
  |<-- version + shuffle table -------|  (2) Handshake response (267 bytes)
  |                                    |
  |<-- BORDER (34 bytes) -------------|  (3) Map boundaries
  |                                    |
  |--- CAPTCHA_TOKEN (JSON) --------->|  (4) Auth
  |                                    |
  |--- SPAWN (JSON + effect) -------->|  (5) Request spawn
  |                                    |
  |<-- SPAWN_RESULT (accepted) -------|  (6) Spawn confirmed
  |<-- ADD_MY_CELL (cellId) ----------|  (7) "This cell is yours"
  |<-- WORLD_UPDATE (full sync) ------|  (8) All existing cells
  |                                    |
  |--- MOUSE (x, y) @ 30Hz --------->|  (9) Continuous mouse updates
  |                                    |
  |<-- WORLD_UPDATE @ 25Hz ----------|  (10) Incremental state diffs
  |<-- CAMERA (x, y) @ 25Hz ---------|  (11) Camera follow
  |<-- LEADERBOARD @ 0.5Hz ----------|  (12) Periodic leaderboard
  |                                    |
  |--- SPLIT ------------------------>|  (13) Space key
  |<-- ADD_MY_CELL (new cells) -------|
  |                                    |
  |--- EJECT ------------------------>|  (14) W key
  |                                    |
  |--- MULTIBOX_TOGGLE -------------->|  (15) Enable multibox
  |<-- MULTIBOX_STATE (on) ----------|
  |<-- ADD_MULTI_CELL (cellId) -------|  (16) Secondary cell spawned
  |                                    |
  |--- MULTIBOX_SWITCH -------------->|  (17) Tab — switch control
  |<-- MULTIBOX_STATE (slot=1) -------|
  |                                    |
  |--- DIRECTION_LOCK (lock=1) ------->|  (18) Shift held — lock heading
  |--- EJECT (aimed elsewhere) ------>|      W key — eject toward cursor
  |--- DIRECTION_LOCK (lock=0) ------>|      Shift released — unlock
  |                                    |
  |--- CHAT (text) ------------------>|  (19) Chat message
  |<-- CHAT_RECV (broadcast) ---------|
  |                                    |
  |--- SPECTATOR_CMD (follow) ------->|  (20) Toggle spectator follow
  |                                    |
  |--- PING ------------------------->|  (21) Latency check
  |<-- PING_REPLY --------------------|
  |                                    |
  |<-- CLEAR_MINE -------------------|  (22) Player died
  |                                    |
  |--- SPAWN (respawn) -------------->|  (23) Respawn
  ...
```

---

## Implementation Notes

### Byte Ordering

All multi-byte values are **little-endian** (LE). JavaScript `DataView` methods with `littleEndian = true`:

```javascript
view.getInt16(offset, true);    // i16 LE
view.getUint16(offset, true);   // u16 LE
view.getInt32(offset, true);    // i32 LE
view.getUint32(offset, true);   // u32 LE
view.getFloat32(offset, true);  // f32 LE
view.getFloat64(offset, true);  // f64 LE
```

### String Encoding

All strings are UTF-8, null-terminated (`\0`). To read:

```javascript
function readString(view, offset) {
  let str = '';
  while (offset < view.byteLength) {
    const byte = view.getUint8(offset++);
    if (byte === 0) break;
    str += String.fromCharCode(byte);
  }
  return { str, offset };
}
```

### Shuffle Table Usage (Client)

```javascript
// After receiving handshake, extract shuffle table (bytes 11–266)
const forward = new Uint8Array(handshake, versionLength, 256);

// Build inverse table
const inverse = new Uint8Array(256);
for (let i = 0; i < 256; i++) {
  inverse[forward[i]] = i;
}

// Decode server message opcode
const logicalOp = inverse[rawBytes[0]];

// Encode client message opcode
const wireByte = forward[logicalOp];
```

### Coordinate System

- Origin (0, 0) is the **center** of the map
- Default map: -7071 to +7071 on both axes (total 14142 × 14142)
- Positive X = right, positive Y = down
- Cell positions in WORLD_UPDATE are truncated to `i16` (±32767 range)

### Mass / Size Relationship

```
mass = size² / 100
size = sqrt(mass × 100)
```

Where `size` is the cell's radius in world units.
