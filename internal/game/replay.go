package game

import (
	"encoding/json"
	"sync"
	"time"
)

// ReplayCell is a lightweight snapshot of one cell for replay.
type ReplayCell struct {
	ID       uint32 `json:"id"`
	X        int16  `json:"x"`
	Y        int16  `json:"y"`
	Size     uint16 `json:"s"`
	R        uint8  `json:"r"`
	G        uint8  `json:"g"`
	B        uint8  `json:"b"`
	IsPlayer bool   `json:"p,omitempty"`
	IsVirus  bool   `json:"v,omitempty"`
	Name     string `json:"n,omitempty"`
	Effect   string `json:"e,omitempty"`
}

// ReplayFrame is one snapshot of the game world.
type ReplayFrame struct {
	Tick      uint64       `json:"tick"`
	Timestamp int64        `json:"ts"` // Unix millis
	Cells     []ReplayCell `json:"cells"`
}

// ReplayBuffer records the last N seconds of game state for replay.
type ReplayBuffer struct {
	mu       sync.RWMutex
	frames   []ReplayFrame
	maxFrames int
	interval  int    // record every N ticks
	tickCount uint64
}

// NewReplayBuffer creates a replay buffer.
// interval: record a frame every N ticks.
// duration: how many seconds of replay to keep.
// E.g., interval=5 at 25Hz = 5 frames/sec, duration=60 → 300 frames.
func NewReplayBuffer(interval int, durationSec int) *ReplayBuffer {
	fps := 25 / interval // ticks per second / interval
	maxFrames := fps * durationSec
	return &ReplayBuffer{
		frames:    make([]ReplayFrame, 0, maxFrames),
		maxFrames: maxFrames,
		interval:  interval,
	}
}

// RecordTick is called every game tick. It only records a frame every
// `interval` ticks for efficiency.
func (rb *ReplayBuffer) RecordTick(tickNum uint64, cells map[uint32]*Cell) {
	rb.tickCount++
	if rb.tickCount%uint64(rb.interval) != 0 {
		return
	}

	// Build compact cell snapshot — skip food for smaller payloads
	snapshot := make([]ReplayCell, 0, len(cells)/2)
	for _, c := range cells {
		if c.Type == CellFood || c.Type == CellEject {
			continue // skip food and ejects to keep replay small
		}
		snapshot = append(snapshot, ReplayCell{
			ID:       c.ID,
			X:        int16(c.X),
			Y:        int16(c.Y),
			Size:     uint16(c.Size),
			R:        c.R,
			G:        c.G,
			B:        c.B,
			IsPlayer: c.IsPlayer,
			IsVirus:  c.IsVirus,
			Name:     c.Name,
			Effect:   c.Effect,
		})
	}

	frame := ReplayFrame{
		Tick:      tickNum,
		Timestamp: time.Now().UnixMilli(),
		Cells:     snapshot,
	}

	rb.mu.Lock()
	if len(rb.frames) >= rb.maxFrames {
		// Shift ring buffer: drop oldest
		copy(rb.frames, rb.frames[1:])
		rb.frames[len(rb.frames)-1] = frame
	} else {
		rb.frames = append(rb.frames, frame)
	}
	rb.mu.Unlock()
}

// GetReplay returns the last N seconds of replay as JSON bytes.
func (rb *ReplayBuffer) GetReplay(lastNSeconds int) ([]byte, error) {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if len(rb.frames) == 0 {
		return json.Marshal([]ReplayFrame{})
	}

	cutoff := time.Now().Add(-time.Duration(lastNSeconds) * time.Second).UnixMilli()
	startIdx := 0
	for i, f := range rb.frames {
		if f.Timestamp >= cutoff {
			startIdx = i
			break
		}
	}

	result := rb.frames[startIdx:]
	return json.Marshal(result)
}

// SnapshotCells returns the engine's cells map under a read lock.
// Caller must not modify the returned map.
func (e *Engine) SnapshotCells() map[uint32]*Cell {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// Copy the map to avoid holding the lock
	snapshot := make(map[uint32]*Cell, len(e.cells))
	for k, v := range e.cells {
		snapshot[k] = v
	}
	return snapshot
}
