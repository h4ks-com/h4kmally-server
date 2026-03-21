package protocol

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
)

// SIG 0.0.2 Protocol Constants
const ProtocolVersion = "SIG 0.0.2"

// Client to Server opcodes (logical)
const (
	OpSpawn          = 0
	OpMouse          = 16
	OpSplit          = 17
	OpEject          = 21
	OpMultiboxToggle = 22 // toggle multibox on/off (1 byte opcode only)
	OpMultiboxSwitch = 23 // switch active slot (1 byte opcode only)
	OpDirectionLock  = 24 // lock/unlock movement direction (payload: 1 byte, 1=lock 0=unlock)
	OpChat           = 99
	OpBoostCheck     = 190
	OpStatUpdate     = 191
	OpFoodEaten      = 192
	OpSpectate       = 205
	OpAdblocker      = 208
	OpCaptchaToken   = 220
	OpPing           = 254
)

// Server to Client opcodes (logical)
const (
	OpWorldUpdate   = 16
	OpCamera        = 17
	OpClearAll      = 18
	OpClearMine     = 20
	OpMultiboxState = 22 // multibox state notification
	OpAddMyCell     = 32
	OpAddMultiCell  = 33 // like AddMyCell but for multi player cells
	OpLeaderboardT  = 48
	OpLeaderboardF  = 49
	OpBorder        = 64
	OpChatRecv      = 99
	OpPasswordErr   = 180
	OpSpawnResult   = 221
	OpPingReply     = 254
)

// ShuffleTable is a 256-byte opcode permutation table.
type ShuffleTable struct {
	Forward [256]byte
	Inverse [256]byte
}

// NewShuffleTable generates a random shuffle table.
func NewShuffleTable() *ShuffleTable {
	st := &ShuffleTable{}
	for i := 0; i < 256; i++ {
		st.Forward[i] = byte(i)
	}
	for i := 255; i > 0; i-- {
		j := rand.IntN(i + 1)
		st.Forward[i], st.Forward[j] = st.Forward[j], st.Forward[i]
	}
	for i := 0; i < 256; i++ {
		st.Inverse[st.Forward[i]] = byte(i)
	}
	return st
}

// Encode returns the shuffled wire opcode for a logical opcode.
func (st *ShuffleTable) Encode(logicalOp byte) byte {
	return st.Forward[logicalOp]
}

// Decode returns the logical opcode for a shuffled wire byte.
func (st *ShuffleTable) Decode(wireOp byte) byte {
	return st.Inverse[wireOp]
}

// EncodeStringUTF8 encodes a string as null-terminated UTF-8 bytes.
func EncodeStringUTF8(s string) []byte {
	b := []byte(s)
	return append(b, 0x00)
}

// DecodeStringUTF8 reads a null-terminated string from buf at offset.
func DecodeStringUTF8(buf []byte, offset int) (string, int) {
	start := offset
	for offset < len(buf) && buf[offset] != 0x00 {
		offset++
	}
	s := string(buf[start:offset])
	if offset < len(buf) {
		offset++
	}
	return s, offset
}

// BuildHandshake builds the server handshake response.
func BuildHandshake(st *ShuffleTable) []byte {
	ver := EncodeStringUTF8(ProtocolVersion)
	buf := make([]byte, len(ver)+256)
	copy(buf, ver)
	copy(buf[len(ver):], st.Forward[:])
	return buf
}

// BuildBorder builds a BORDER message.
func BuildBorder(st *ShuffleTable, left, top, right, bottom float64) []byte {
	buf := make([]byte, 33)
	buf[0] = st.Encode(OpBorder)
	binary.LittleEndian.PutUint64(buf[1:], math.Float64bits(left))
	binary.LittleEndian.PutUint64(buf[9:], math.Float64bits(top))
	binary.LittleEndian.PutUint64(buf[17:], math.Float64bits(right))
	binary.LittleEndian.PutUint64(buf[25:], math.Float64bits(bottom))
	return buf
}

// BuildAddMyCell builds an ADD_MY_CELL message.
func BuildAddMyCell(st *ShuffleTable, cellID uint32) []byte {
	buf := make([]byte, 5)
	buf[0] = st.Encode(OpAddMyCell)
	binary.LittleEndian.PutUint32(buf[1:], cellID)
	return buf
}

// BuildAddMultiCell builds an ADD_MULTI_CELL message.
func BuildAddMultiCell(st *ShuffleTable, cellID uint32) []byte {
	buf := make([]byte, 5)
	buf[0] = st.Encode(OpAddMultiCell)
	binary.LittleEndian.PutUint32(buf[1:], cellID)
	return buf
}

// BuildClearMine builds a CLEAR_MINE message.
func BuildClearMine(st *ShuffleTable) []byte {
	return []byte{st.Encode(OpClearMine)}
}

// BuildClearAll builds a CLEAR_ALL message.
func BuildClearAll(st *ShuffleTable) []byte {
	return []byte{st.Encode(OpClearAll)}
}

// BuildSpawnResult builds a SPAWN_RESULT message.
func BuildSpawnResult(st *ShuffleTable, accepted bool) []byte {
	buf := make([]byte, 2)
	buf[0] = st.Encode(OpSpawnResult)
	if accepted {
		buf[1] = 1
	}
	return buf
}

// BuildPingReply builds a PING_REPLY message.
func BuildPingReply(st *ShuffleTable) []byte {
	return []byte{st.Encode(OpPingReply)}
}

// BuildCamera builds a CAMERA message with position and zoom.
func BuildCamera(st *ShuffleTable, x, y, zoom float32) []byte {
	buf := make([]byte, 13)
	buf[0] = st.Encode(OpCamera)
	binary.LittleEndian.PutUint32(buf[1:], math.Float32bits(x))
	binary.LittleEndian.PutUint32(buf[5:], math.Float32bits(y))
	binary.LittleEndian.PutUint32(buf[9:], math.Float32bits(zoom))
	return buf
}

// BuildMultiboxState builds a MULTIBOX_STATE message.
// enabled=1/0, activeSlot=0/1, multiAlive=1/0
func BuildMultiboxState(st *ShuffleTable, enabled bool, activeSlot byte, multiAlive bool) []byte {
	buf := make([]byte, 4)
	buf[0] = st.Encode(OpMultiboxState)
	if enabled {
		buf[1] = 1
	}
	buf[2] = activeSlot
	if multiAlive {
		buf[3] = 1
	}
	return buf
}

// BuildChat builds a server CHAT message.
func BuildChat(st *ShuffleTable, flags byte, r, g, b uint8, name, msg string) []byte {
	nameBytes := EncodeStringUTF8(name)
	msgBytes := EncodeStringUTF8(msg)
	buf := make([]byte, 5+len(nameBytes)+len(msgBytes))
	buf[0] = st.Encode(OpChatRecv)
	buf[1] = flags
	buf[2] = r
	buf[3] = g
	buf[4] = b
	copy(buf[5:], nameBytes)
	copy(buf[5+len(nameBytes):], msgBytes)
	return buf
}
