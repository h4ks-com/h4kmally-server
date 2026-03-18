# SIG 0.0.1 Protocol Specification

The SIG 0.0.1 protocol is a binary WebSocket protocol used for communication between the h4kmally game client and server. All messages are sent as binary WebSocket frames. Multi-byte integers are **little-endian**.

## Table of Contents

- [Connection Handshake](#connection-handshake)
- [Opcode Shuffling](#opcode-shuffling)
- [Data Types](#data-types)
- [Client → Server Messages](#client--server-messages)
- [Server → Client Messages](#server--client-messages)
- [World Update Format](#world-update-format)
- [Typical Message Flow](#typical-message-flow)

---

## Connection Handshake

### Step 1: Client sends protocol version

The client sends a UTF-8 null-terminated string:

```
"SIG 0.0.1\0"
```

### Step 2: Server responds with version + shuffle table

```
Offset  Size     Description
0       var      "SIG 0.0.1\0" — null-terminated UTF-8 string (10 bytes)
10      256      Shuffle table — a byte permutation (0–255 shuffled)
```

Total: **266 bytes**

The shuffle table is a random permutation of bytes 0–255. It is used to obfuscate opcodes on the wire (see [Opcode Shuffling](#opcode-shuffling)).

### Step 3: Server sends BORDER

Immediately after the handshake, the server sends a `BORDER` message defining the map boundaries. The message must be at least **34 bytes** (33 bytes of border data + 1 padding byte) to trigger the client's ping loop.

---

## Opcode Shuffling

All opcodes are shuffled through a per-connection permutation table to prevent trivial packet sniffing.

- **Client → Server**: The client receives the shuffle table `T[256]` and uses the **inverse** mapping. When sending logical opcode `op`, the wire byte is `T.inverse[op]`. The server decodes with `T.forward[wire_byte]` → but in practice, the server stores inverse = undo of forward, so: `T.inverse[wire_byte]` gives back the logical opcode.
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
  "showClanmates": false,
  "token": "auth_token",
  "email": "user@example.com"
}
```

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

### CHAT (opcode 99)

Sends a chat message.

```
Offset  Type    Field
0       u8      opcode (shuffled 99)
1       u8      flags (0 = normal)
2       string  message text (null-terminated)
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

### ADD_MY_CELL (opcode 32)

Notifies the client that a cell belongs to them.

```
Offset  Type    Field
0       u8      opcode (shuffled 32)
1       u32     cellId — the cell ID to claim
```

Total: **5 bytes**

Sent after spawning and after splitting to inform the client which cells are "mine" (rendered differently, shown on minimap, etc.).

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

Client measures round-trip time between sending PING and receiving PING_REPLY.

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

  End sentinel:
    +0      u32     0x00000000  (cellId = 0 marks end of cell updates)

Section 4: Removals
  +0      u16     removeCount — number of cells removed
  For each removal:
    +0      u32     cellId
```

### Cell Update Flags

The flags byte controls which optional fields are present:

| Bit  | Value | Field       | When set                                |
|------|-------|-------------|-----------------------------------------|
| 1    | 0x02  | Color       | 3 bytes (R, G, B) follow after clan     |
| 2    | 0x04  | Skin        | Null-terminated skin string follows      |
| 3    | 0x08  | Name        | Null-terminated name string follows      |

Flags are only set when the data has changed (dirty). On first appearance, all flags are typically set. On subsequent updates (position/size changes), flags may be 0 if color/skin/name haven't changed.

### Cell Types in Updates

| isVirus | isPlayer | Type      | Visual                          |
|---------|----------|-----------|---------------------------------|
| 0       | 0        | Food/Eject| Small colored circle            |
| 0       | 1        | Player    | Large circle with name + outline |
| 1       | 0        | Virus     | Semi-transparent green, spiky    |

---

## Typical Message Flow

```
Client                              Server
  |                                    |
  |--- "SIG 0.0.1\0" --------------->|  (1) Protocol version
  |                                    |
  |<-- version + shuffle table -------|  (2) Handshake response (266 bytes)
  |                                    |
  |<-- BORDER (34 bytes) -------------|  (3) Map boundaries
  |                                    |
  |--- CAPTCHA_TOKEN (JSON) --------->|  (4) Auth (accepted by default)
  |                                    |
  |--- SPAWN (JSON) ----------------->|  (5) Request spawn
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
  |--- CHAT (text) ------------------>|  (15) Chat message
  |<-- CHAT_RECV (broadcast) ---------|
  |                                    |
  |--- PING ------------------------->|  (16) Latency check
  |<-- PING_REPLY --------------------|
  |                                    |
  |<-- CLEAR_MINE -------------------|  (17) Player died
  |                                    |
  |--- SPAWN (respawn) -------------->|  (18) Respawn
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
// After receiving handshake, extract shuffle table (bytes 10–265)
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
