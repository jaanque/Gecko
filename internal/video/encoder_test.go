package video

import (
	"encoding/binary"
	"image"
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

func TestTileEncoderDirtyRectsFastPath(t *testing.T) {
	const w, h = 192, 128 // 3x2 grid of 64x64 tiles
	enc := NewTileEncoder()
	defer enc.Close()

	frame := make([]byte, w*h*4)
	for i := 0; i < len(frame); i += 4 {
		frame[i+0], frame[i+1], frame[i+2], frame[i+3] = 200, 200, 200, 255
	}

	// First frame encodes all 6 tiles regardless of dirty rects.
	pkt1 := enc.EncodeDirtyRects(1, frame, w, h, 78, []image.Rectangle{image.Rect(0, 0, 10, 10)}, false)
	if tiles := binary.BigEndian.Uint16(pkt1[8:10]); tiles != 6 {
		t.Fatalf("expected 6 tiles on first frame, got %d", tiles)
	}

	// Mutate pixels in bottom-right tile (130, 70) and pass matching GPU dirty rect.
	off := (70*w + 130) * 4
	frame[off] = 42
	pkt2 := enc.EncodeDirtyRects(2, frame, w, h, 78, []image.Rectangle{
		image.Rect(128, 64, 160, 96),
		image.Rect(130, 70, 150, 90), // overlapping rect must be deduplicated
	}, false)
	if pkt2 == nil {
		t.Fatal("expected dirty tile packet")
	}
	if tiles := binary.BigEndian.Uint16(pkt2[8:10]); tiles != 1 {
		t.Fatalf("expected exactly 1 deduplicated dirty tile, got %d", tiles)
	}

	// Re-encoding same dirty rect with unchanged pixels emits nil.
	if pkt3 := enc.EncodeDirtyRects(3, frame, w, h, 78, []image.Rectangle{image.Rect(128, 64, 160, 96)}, false); pkt3 != nil {
		t.Fatalf("expected nil for already-synced dirty rect, got %d bytes", len(pkt3))
	}
}
