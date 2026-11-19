package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := AuthPayload{
		Token:    "secret-token",
		Hostname: "PC-TEST-01",
		Width:    1920,
		Height:   1080,
	}
	if err := WriteJSON(&buf, MsgAuth, want); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	msgType, payload, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if msgType != MsgAuth || len(payload) == 0 {
		t.Fatalf("unexpected message: type=%d len=%d", msgType, len(payload))
	}
}

func TestBinaryEventCodecs(t *testing.T) {
	m := MouseEvent{Action: MouseWheel, Button: ButtonRight, X: 32768, Y: 65535, Delta: -120}
	m2, err := DecodeMouse(EncodeMouse(m))
	if err != nil || m != m2 {
		t.Fatalf("mouse mismatch: got %+v err=%v", m2, err)
	}

	k := KeyEvent{Action: KeyDown, VK: 0x41, Scan: 0x1E, Flags: KeyFlagExtended}
	k2, err := DecodeKey(EncodeKey(k))
	if err != nil || k != k2 {
		t.Fatalf("key mismatch: got %+v err=%v", k2, err)
	}

	ack2, err := DecodeFrameAck(EncodeFrameAck(42))
	if err != nil || ack2 != 42 {
		t.Fatalf("ack mismatch: got %d err=%v", ack2, err)
	}
}

func TestStrictMessageAndEventValidation(t *testing.T) {
	// Unknown message type must be rejected before reading payload.
	var badType [HeaderSize]byte
	badType[0] = 0xFE
	binary.BigEndian.PutUint32(badType[1:5], 10)
	if _, _, err := ReadMessage(bytes.NewReader(badType[:])); err == nil {
		t.Fatal("expected unknown message type to be rejected")
	}

	// Oversized control message (e.g. MsgAuth claiming 1 MiB) must be rejected immediately.
	var bigAuth [HeaderSize]byte
	bigAuth[0] = MsgAuth
	binary.BigEndian.PutUint32(bigAuth[1:5], 1024*1024)
	if _, _, err := ReadMessage(bytes.NewReader(bigAuth[:])); err == nil {
		t.Fatal("expected oversized MsgAuth to be rejected")
	}

	// Oversized MsgMouse (> 8 bytes) must be rejected immediately.
	var bigMouse [HeaderSize]byte
	bigMouse[0] = MsgMouse
	binary.BigEndian.PutUint32(bigMouse[1:5], 16)
	if _, _, err := ReadMessage(bytes.NewReader(bigMouse[:])); err == nil {
		t.Fatal("expected oversized MsgMouse to be rejected")
	}

	// Invalid mouse action / button must be rejected.
	if _, err := DecodeMouse(EncodeMouse(MouseEvent{Action: 99, Button: ButtonLeft})); err == nil {
		t.Fatal("expected invalid mouse action to fail")
	}
	if _, err := DecodeMouse(EncodeMouse(MouseEvent{Action: MouseDown, Button: 99})); err == nil {
		t.Fatal("expected invalid mouse button to fail")
	}

	// Invalid key action / VK / flags must be rejected.
	if _, err := DecodeKey(EncodeKey(KeyEvent{Action: 5, VK: 0x41})); err == nil {
		t.Fatal("expected invalid key action to fail")
	}
	if _, err := DecodeKey(EncodeKey(KeyEvent{Action: KeyDown, VK: 0})); err == nil {
		t.Fatal("expected VK=0 to fail")
	}
	if _, err := DecodeKey(EncodeKey(KeyEvent{Action: KeyDown, VK: 0x41, Flags: 0x80})); err == nil {
		t.Fatal("expected unknown key flags to fail")
	}
}

func TestFileChunkAndSanitizeFileName(t *testing.T) {
	raw, err := EncodeFileChunk(FileChunk{
		TransferID: "tx-1234",
		Offset:     65536,
		Data:       []byte("hello-world"),
	})
	if err != nil {
		t.Fatalf("EncodeFileChunk failed: %v", err)
	}
	got, err := DecodeFileChunk(raw)
	if err != nil {
		t.Fatalf("DecodeFileChunk failed: %v", err)
	}
	if got.TransferID != "tx-1234" || got.Offset != 65536 || string(got.Data) != "hello-world" {
		t.Fatalf("unexpected chunk: %+v", got)
	}

	if _, err := DecodeFileChunk([]byte{0, 1, 2}); err == nil {
		t.Fatal("expected short chunk to fail")
	}

	cases := map[string]string{
		"../../etc/passwd":            "passwd",
		`C:\Windows\System32\cmd.exe`: "cmd.exe",
		"reporte:final?.pdf":          "reportefinal.pdf",
		"...":                         "archivo.bin",
		"informe 2026.txt":            "informe 2026.txt",
	}
	for in, want := range cases {
		if got := SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorPacketRoundTrip(t *testing.T) {
	// 1. Position only (no shape payload)
	p1 := CursorPacket{
		X:         100,
		Y:         200,
		HotspotX:  2,
		HotspotY:  4,
		Width:     32,
		Height:    32,
		Visible:   true,
		ShapeHash: 0xDEADBEEF,
	}
	raw1, err := EncodeCursor(p1)
	if err != nil {
		t.Fatalf("EncodeCursor failed: %v", err)
	}
	if len(raw1) != CursorHeaderSize {
		t.Fatalf("len(raw1) = %d, want %d", len(raw1), CursorHeaderSize)
	}
	d1, err := DecodeCursor(raw1)
	if err != nil {
		t.Fatalf("DecodeCursor failed: %v", err)
	}
	if d1.X != p1.X || d1.Y != p1.Y || !d1.Visible || d1.ShapeHash != p1.ShapeHash || len(d1.ShapeBGRA) != 0 {
		t.Fatalf("decoded mismatch: %+v", d1)
	}

	// 2. Position + Shape payload
	fakeShape := make([]byte, 32*32*4)
	fakeShape[0] = 0xAA
	fakeShape[len(fakeShape)-1] = 0xBB
	p2 := CursorPacket{
		X:         500,
		Y:         600,
		HotspotX:  16,
		HotspotY:  16,
		Width:     32,
		Height:    32,
		Visible:   true,
		ShapeHash: 0x12345678,
		ShapeBGRA: fakeShape,
	}
	raw2, err := EncodeCursor(p2)
	if err != nil {
		t.Fatalf("EncodeCursor shape failed: %v", err)
	}
	d2, err := DecodeCursor(raw2)
	if err != nil {
		t.Fatalf("DecodeCursor shape failed: %v", err)
	}
	if d2.ShapeHash != p2.ShapeHash || len(d2.ShapeBGRA) != len(fakeShape) {
		t.Fatalf("decoded shape mismatch: len=%d", len(d2.ShapeBGRA))
	}
	if !bytes.Equal(d2.ShapeBGRA, fakeShape) {
		t.Fatal("decoded shape bytes do not match")
	}
}
