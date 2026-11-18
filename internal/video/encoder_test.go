package video

import (
	"encoding/binary"
	"testing"

	"remote-control/internal/protocol"
)

func TestTileEncoderDirtyAndKeyframe(t *testing.T) {
	const w, h = 128, 128
	enc := NewTileEncoder()
	defer enc.Close()

	frame := make([]byte, w*h*4)
	for i := 0; i < len(frame); i += 4 {
		frame[i+0] = 240
		frame[i+1] = 240
		frame[i+2] = 240
		frame[i+3] = 255
	}

	// First frame sends all 4 tiles (2x2 grid of 64x64).
	pkt1 := enc.Encode(1, frame, w, h, 78, false)
	if len(pkt1) < protocol.FrameHeaderSize {
		t.Fatalf("expected non-empty keyframe")
	}
	if tiles := binary.BigEndian.Uint16(pkt1[8:10]); tiles != 4 {
		t.Fatalf("expected 4 tiles on first frame, got %d", tiles)
	}

	// Identical frame should produce nil (no dirty tiles).
	if pkt2 := enc.Encode(2, frame, w, h, 78, false); pkt2 != nil {
		t.Fatalf("expected nil for unchanged frame, got %d bytes", len(pkt2))
	}

	// Mutate a single pixel in top-left tile -> only 1 dirty tile.
	frame[0] = 10
	pkt3 := enc.Encode(3, frame, w, h, 78, false)
	if len(pkt3) < protocol.FrameHeaderSize {
		t.Fatalf("expected dirty frame")
	}
	if tiles := binary.BigEndian.Uint16(pkt3[8:10]); tiles != 1 {
		t.Fatalf("expected 1 dirty tile, got %d", tiles)
	}

	// Force keyframe -> all 4 tiles again.
	pkt4 := enc.Encode(4, frame, w, h, 78, true)
	if tiles := binary.BigEndian.Uint16(pkt4[8:10]); tiles != 4 {
		t.Fatalf("expected 4 tiles on forced keyframe, got %d", tiles)
	}
}
