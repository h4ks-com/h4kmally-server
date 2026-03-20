package protocol

import (
	"encoding/binary"
	"encoding/json"
	"log"
)

// ParsedMessage represents a decoded client message.
type ParsedMessage struct {
	Op      byte
	Payload []byte // raw payload (after opcode)
}

// ParseClientMessage decodes a raw WebSocket binary frame using the shuffle table.
func ParseClientMessage(st *ShuffleTable, raw []byte) *ParsedMessage {
	if len(raw) == 0 {
		return nil
	}
	return &ParsedMessage{
		Op:      st.Decode(raw[0]),
		Payload: raw[1:],
	}
}

// ParseSpawnPayload parses the JSON payload of a SPAWN message.
type SpawnPayload struct {
	Name          string `json:"name"`
	Skin          string `json:"skin"`
	Effect        string `json:"effect"`
	ShowClanmates bool   `json:"showClanmates"`
	Token         string `json:"token"`
	Email         string `json:"email"`
}

func ParseSpawn(payload []byte) (*SpawnPayload, error) {
	// Payload is null-terminated JSON string
	s, _ := DecodeStringUTF8(payload, 0)
	var sp SpawnPayload
	if err := json.Unmarshal([]byte(s), &sp); err != nil {
		return nil, err
	}
	return &sp, nil
}

// ParseMouse parses the mouse position payload.
func ParseMouse(payload []byte) (x, y int32, ok bool) {
	if len(payload) < 8 {
		return 0, 0, false
	}
	x = int32(binary.LittleEndian.Uint32(payload[0:4]))
	y = int32(binary.LittleEndian.Uint32(payload[4:8]))
	return x, y, true
}

// ParseChat parses a chat message payload.
func ParseChat(payload []byte) (flags byte, msg string, ok bool) {
	if len(payload) < 2 {
		return 0, "", false
	}
	flags = payload[0]
	msg, _ = DecodeStringUTF8(payload, 1)
	return flags, msg, true
}

// ParseCaptchaToken parses the captcha token payload.
type CaptchaPayload struct {
	Token string `json:"token"`
}

func ParseCaptchaToken(payload []byte) (*CaptchaPayload, error) {
	s, _ := DecodeStringUTF8(payload, 0)
	var cp CaptchaPayload
	if err := json.Unmarshal([]byte(s), &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// HandleMessage processes a single client message and returns any response.
// This is the main dispatch function.
func HandleMessage(msg *ParsedMessage) {
	// Stub — actual handling is in the connection handler
	log.Printf("Unhandled opcode: %d", msg.Op)
}
