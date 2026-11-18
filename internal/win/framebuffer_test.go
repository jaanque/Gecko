package win

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"remote-control/internal/protocol"
)

func TestFrameBufferApplyHybridTiles(t *testing.T) {
	// Tile 1 (JPEG): blue-ish 16x16 at (0, 0).
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

	// Tile 2 (Lossless Zlib-BGR): exact RGB (210, 45, 90) 16x16 at (32, 16).
	bgrRaw := make([]byte, 16*16*3)
	for i := 0; i < len(bgrRaw); i += 3 {
		bgrRaw[i+0] = 90  // B
		bgrRaw[i+1] = 45  // G
		bgrRaw[i+2] = 210 // R
	}
	var zbuf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zbuf, zlib.BestSpeed)
	_, _ = zw.Write(bgrRaw)
	_ = zw.Close()

	var pkt bytes.Buffer
	var hdr [protocol.FrameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 7)
	binary.BigEndian.PutUint16(hdr[4:6], 64)
	binary.BigEndian.PutUint16(hdr[6:8], 64)
	binary.BigEndian.PutUint16(hdr[8:10], 2)
	pkt.Write(hdr[:])

	var thdr [protocol.TileHeaderSize]byte
	// Write JPEG tile
	binary.BigEndian.PutUint16(thdr[0:2], 0)
	binary.BigEndian.PutUint16(thdr[2:4], 0)
	binary.BigEndian.PutUint16(thdr[4:6], 16)
	binary.BigEndian.PutUint16(thdr[6:8], 16)
	thdr[8] = protocol.TileCodecJPEG
	binary.BigEndian.PutUint32(thdr[9:13], uint32(jpg.Len()))
	pkt.Write(thdr[:])
	pkt.Write(jpg.Bytes())

	// Write Zlib-BGR lossless tile
	binary.BigEndian.PutUint16(thdr[0:2], 32)
	binary.BigEndian.PutUint16(thdr[2:4], 16)
	binary.BigEndian.PutUint16(thdr[4:6], 16)
	binary.BigEndian.PutUint16(thdr[6:8], 16)
	thdr[8] = protocol.TileCodecZlibBGR
	binary.BigEndian.PutUint32(thdr[9:13], uint32(zbuf.Len()))
	pkt.Write(thdr[:])
	pkt.Write(zbuf.Bytes())

	fb := NewFrameBuffer()
	meta, err := fb.ApplyFrame(pkt.Bytes())
	if err != nil {
		t.Fatalf("ApplyFrame failed: %v", err)
	}
	if meta.FrameID != 7 || meta.Width != 64 || meta.Height != 64 {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	fb.WithReadLock(func(width, height int, bgra []byte) {
		// Verify JPEG tile pixel at (8, 8)
		off1 := (8*width + 8) * 4
		if bgra[off1+3] != 255 || bgra[off1+0] < 180 || bgra[off1+1] < 90 || bgra[off1+2] > 50 {
			t.Fatalf("unexpected JPEG BGRA pixel: B=%d G=%d R=%d A=%d", bgra[off1], bgra[off1+1], bgra[off1+2], bgra[off1+3])
		}
		// Verify lossless Zlib-BGR tile pixel at (40, 24) is bit-exact!
		off2 := (24*width + 40) * 4
		if bgra[off2+0] != 90 || bgra[off2+1] != 45 || bgra[off2+2] != 210 || bgra[off2+3] != 255 {
			t.Fatalf("expected exact lossless BGRA (90,45,210,255), got (%d,%d,%d,%d)", bgra[off2], bgra[off2+1], bgra[off2+2], bgra[off2+3])
		}
	})
}

func TestFrameBufferRejectsMalformedAndBombFrames(t *testing.T) {
	fb := NewFrameBuffer()

	// 1. Short packet
	if _, err := fb.ApplyFrame([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for short frame")
	}

	// 2. Oversized dimension bomb (16384x16384 would allocate 1 GiB)
	var dimBomb [protocol.FrameHeaderSize]byte
	binary.BigEndian.PutUint16(dimBomb[4:6], 16384)
	binary.BigEndian.PutUint16(dimBomb[6:8], 16384)
	binary.BigEndian.PutUint16(dimBomb[8:10], 1)
	if _, err := fb.ApplyFrame(dimBomb[:]); err == nil {
		t.Fatal("expected oversized dimensions to be rejected")
	}

	// 3. Tile count bomb (65535 tiles claimed in 10-byte packet)
	var tileBomb [protocol.FrameHeaderSize]byte
	binary.BigEndian.PutUint16(tileBomb[4:6], 64)
	binary.BigEndian.PutUint16(tileBomb[6:8], 64)
	binary.BigEndian.PutUint16(tileBomb[8:10], 65535)
	if _, err := fb.ApplyFrame(tileBomb[:]); err == nil {
		t.Fatal("expected excessive tile count to be rejected")
	}

	// 4. Truncated tile + integer-overflow dataLen (0xFFFFFFFF) must not panic.
	var badTile [protocol.FrameHeaderSize + protocol.TileHeaderSize + 4]byte
	binary.BigEndian.PutUint16(badTile[4:6], 64)
	binary.BigEndian.PutUint16(badTile[6:8], 64)
	binary.BigEndian.PutUint16(badTile[8:10], 1)
	binary.BigEndian.PutUint16(badTile[14:16], 16)
	binary.BigEndian.PutUint16(badTile[16:18], 16)
	badTile[18] = protocol.TileCodecZlibBGR
	binary.BigEndian.PutUint32(badTile[19:23], 0xFFFFFFFF)
	if _, err := fb.ApplyFrame(badTile[:]); err != nil {
		t.Fatalf("truncated tile should be safely ignored without panic, got err=%v", err)
	}
}
