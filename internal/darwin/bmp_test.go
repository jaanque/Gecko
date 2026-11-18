package darwin

import (
	"encoding/binary"
	"testing"
)

func TestDecodeBMPToBGRA(t *testing.T) {
	// Construct a tiny 2x2 bottom-up 24-bit BMP (row stride padded to 8 bytes).
	buf := make([]byte, 54+16)
	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[10:14], 54)
	binary.LittleEndian.PutUint32(buf[14:18], 40)
	binary.LittleEndian.PutUint32(buf[18:22], 2)
	binary.LittleEndian.PutUint32(buf[22:26], 2)
	binary.LittleEndian.PutUint16(buf[26:28], 1)
	binary.LittleEndian.PutUint16(buf[28:30], 24)

	// Bottom row (y=1): pixel (0,1) = B:10 G:20 R:30
	buf[54+0], buf[54+1], buf[54+2] = 10, 20, 30
	// Top row (y=0): pixel (0,0) = B:100 G:150 R:200
	buf[54+8+0], buf[54+8+1], buf[54+8+2] = 100, 150, 200

	bgra, w, h, err := decodeBMPToBGRA(buf, nil)
	if err != nil {
		t.Fatalf("decodeBMPToBGRA failed: %v", err)
	}
	if w != 2 || h != 2 || len(bgra) != 16 {
		t.Fatalf("unexpected size %dx%d len=%d", w, h, len(bgra))
	}
	if bgra[0] != 100 || bgra[1] != 150 || bgra[2] != 200 || bgra[3] != 255 {
		t.Fatalf("top-left pixel mismatch: %v", bgra[:4])
	}
	if bgra[8] != 10 || bgra[9] != 20 || bgra[10] != 30 || bgra[11] != 255 {
		t.Fatalf("bottom-left pixel mismatch: %v", bgra[8:12])
	}
}
