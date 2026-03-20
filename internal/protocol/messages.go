package protocol

import (
	"github.com/h4ks-com/h4kmally-server/internal/game"
)

// WorldUpdateBuilder constructs WORLD_UPDATE packets incrementally.
type WorldUpdateBuilder struct {
	buf []byte
	st  *ShuffleTable
}

// NewWorldUpdateBuilder creates a builder for a world update message.
func NewWorldUpdateBuilder(st *ShuffleTable, eatEvents []game.EatEvent) *WorldUpdateBuilder {
	// Pre-allocate: opcode(1) + eatCount(2) + eats(8 each) + some room
	estSize := 3 + len(eatEvents)*8 + 1024
	buf := make([]byte, 0, estSize)

	// Opcode
	buf = append(buf, st.Encode(OpWorldUpdate))

	// Eat events count
	buf = appendU16(buf, uint16(len(eatEvents)))

	// Eat events
	for _, ev := range eatEvents {
		buf = appendU32(buf, ev.EaterID)
		buf = appendU32(buf, ev.EatenID)
	}

	return &WorldUpdateBuilder{buf: buf, st: st}
}

// AddCell writes a cell update entry.
// Always includes color. Includes skin/name if non-empty.
func (w *WorldUpdateBuilder) AddCell(c *game.Cell) {
	w.buf = appendU32(w.buf, c.ID)
	w.buf = appendI16(w.buf, int16(c.X))
	w.buf = appendI16(w.buf, int16(c.Y))
	w.buf = appendU16(w.buf, uint16(c.Size))

	// Always send color; include skin/name if non-empty
	var flags byte = 0x02 // always include color
	if c.Skin != "" {
		flags |= 0x04
	}
	if c.Name != "" {
		flags |= 0x08
	}
	if c.Effect != "" {
		flags |= 0x10
	}

	w.buf = append(w.buf, flags)

	// isVirus, isPlayer, isSubscriber
	w.buf = append(w.buf, boolByte(c.IsVirus))
	w.buf = append(w.buf, boolByte(c.IsPlayer))
	w.buf = append(w.buf, boolByte(c.IsSubscriber))

	// Clan (null-terminated, can be empty)
	w.buf = append(w.buf, EncodeStringUTF8(c.Clan)...)

	// Color (always)
	w.buf = append(w.buf, c.R, c.G, c.B)

	// Skin (if non-empty)
	if flags&0x04 != 0 {
		w.buf = append(w.buf, EncodeStringUTF8(c.Skin)...)
	}
	// Name (if non-empty)
	if flags&0x08 != 0 {
		w.buf = append(w.buf, EncodeStringUTF8(c.Name)...)
	}
	// Effect (if non-empty)
	if flags&0x10 != 0 {
		w.buf = append(w.buf, EncodeStringUTF8(c.Effect)...)
	}
}

// Finish writes the end sentinel and deletion section, returning the final message.
func (w *WorldUpdateBuilder) Finish(removed []uint32) []byte {
	// End sentinel: cell ID 0
	w.buf = appendU32(w.buf, 0)

	// Deletion count
	w.buf = appendU16(w.buf, uint16(len(removed)))

	// Deletion IDs
	for _, id := range removed {
		w.buf = appendU32(w.buf, id)
	}

	return w.buf
}

// BuildLeaderboardFFA builds a FFA leaderboard message.
func BuildLeaderboardFFA(st *ShuffleTable, entries []game.LeaderEntry, myName string) []byte {
	buf := make([]byte, 0, 256)
	buf = append(buf, st.Encode(OpLeaderboardF))
	buf = appendU32(buf, uint32(len(entries)))

	for i, e := range entries {
		// "me" flag
		isMe := uint32(0)
		if e.Name == myName {
			isMe = 1
		}
		buf = appendU32(buf, isMe)
		buf = append(buf, EncodeStringUTF8(e.Name)...)
		buf = appendU32(buf, uint32(i+1)) // rank (1-based)
		sub := uint32(0)
		if e.IsSubscriber {
			sub = 1
		}
		buf = appendU32(buf, sub)
	}

	return buf
}

// --- Helpers ---

func appendU16(buf []byte, v uint16) []byte {
	return append(buf, byte(v), byte(v>>8))
}

func appendU32(buf []byte, v uint32) []byte {
	return append(buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendI16(buf []byte, v int16) []byte {
	return appendU16(buf, uint16(v))
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
