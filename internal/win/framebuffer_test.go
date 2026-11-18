package win

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestFrameBufferApplyTile(t *testing.T) {
	// Create a solid blue-ish 16x16 tile and encode as JPEG.
	tile := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			tile.SetRGBA(x, y, color.RGBA{R: 20, G: 120, B: 220, A: 255})
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, tile, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode failed: %v", err)
	}

	var pkt bytes.Buffer
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[0:2], 64)
	binary.BigEndian.PutUint16(hdr[2:4], 64)
	binary.BigEndian.PutUint16(hdr[4:6], 1)
	pkt.Write(hdr[:])

	var thdr [12]byte
	binary.BigEndian.PutUint16(thdr[0:2], 16)
	binary.BigEndian.PutUint16(thdr[2:4], 16)
	binary.BigEndian.PutUint16(thdr[4:6], 16)
	binary.BigEndian.PutUint16(thdr[6:8], 16)
	binary.BigEndian.PutUint32(thdr[8:12], uint32(jpg.Len()))
	pkt.Write(thdr[:])
	pkt.Write(jpg.Bytes())

	fb := NewFrameBuffer()
	w, h, err := fb.ApplyFrame(pkt.Bytes())
	if err != nil {
		t.Fatalf("ApplyFrame failed: %v", err)
	}
	if w != 64 || h != 64 {
		t.Fatalf("expected 64x64, got %dx%d", w, h)
	}

	fb.WithReadLock(func(width, height int, bgra []byte) {
		// Center of the blitted tile at (24, 24) should have B > G > R.
		off := (24*width + 24) * 4
		b, g, r, a := bgra[off], bgra[off+1], bgra[off+2], bgra[off+3]
		if a != 255 || b < 180 || g < 90 || r > 50 {
			t.Fatalf("unexpected BGRA pixel at (24,24): B=%d G=%d R=%d A=%d", b, g, r, a)
		}
	})
}
