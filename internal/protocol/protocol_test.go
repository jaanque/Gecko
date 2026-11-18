package protocol

import (
	"bytes"
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
