package adminweb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"net"
	"testing"

	"remote-control/internal/protocol"
)

func TestWebSocketRoundTripMaskedAndUnmasked(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	serverWS := &wsConn{conn: c1, br: bufio.NewReader(c1)}

	// Simulate a masked browser client frame ("hello-gecko").
	msg := []byte("hello-gecko")
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	frame := make([]byte, 2+4+len(msg))
	frame[0] = 0x80 | wsOpText
	frame[1] = 0x80 | byte(len(msg))
	copy(frame[2:6], mask[:])
	for i, b := range msg {
		frame[6+i] = b ^ mask[i%4]
	}

	go func() {
		_, _ = c2.Write(frame)
	}()

	op, got, err := serverWS.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if op != wsOpText || string(got) != "hello-gecko" {
		t.Fatalf("unexpected frame: op=%d data=%q", op, string(got))
	}
}

func TestTranscodeFrameForBrowser(t *testing.T) {
	// Build a 1-tile Zlib-BGR MsgFrame and confirm it transcodes to browser codec 2 (raw RGBA).
	bgr := []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120} // 2x2 BGR
	var zbuf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zbuf, zlib.BestSpeed)
	_, _ = zw.Write(bgr)
	_ = zw.Close()

	var pkt bytes.Buffer
	var hdr [protocol.FrameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 9)
	binary.BigEndian.PutUint16(hdr[4:6], 64)
	binary.BigEndian.PutUint16(hdr[6:8], 64)
	binary.BigEndian.PutUint16(hdr[8:10], 1)
	pkt.Write(hdr[:])

	var th [protocol.TileHeaderSize]byte
	binary.BigEndian.PutUint16(th[0:2], 4)
	binary.BigEndian.PutUint16(th[2:4], 8)
	binary.BigEndian.PutUint16(th[4:6], 2)
	binary.BigEndian.PutUint16(th[6:8], 2)
	th[8] = protocol.TileCodecZlibBGR
	binary.BigEndian.PutUint32(th[9:13], uint32(zbuf.Len()))
	pkt.Write(th[:])
	pkt.Write(zbuf.Bytes())

	out, frameID, ok := transcodeFrameForBrowser(pkt.Bytes())
	if !ok || frameID != 9 {
		t.Fatalf("transcodeFrameForBrowser failed: ok=%v id=%d", ok, frameID)
	}
	if out[protocol.FrameHeaderSize+8] != browserCodecRGBA {
		t.Fatalf("expected browserCodecRGBA (%d), got %d", browserCodecRGBA, out[protocol.FrameHeaderSize+8])
	}
	rgba := out[protocol.FrameHeaderSize+protocol.TileHeaderSize:]
	if len(rgba) != 16 || rgba[0] != 30 || rgba[1] != 20 || rgba[2] != 10 || rgba[3] != 255 {
		t.Fatalf("unexpected RGBA pixel 0: %v", rgba[:4])
	}
}
