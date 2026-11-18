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
		OS:       "windows",
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
	if msgType != MsgAuth {
		t.Fatalf("expected type %d, got %d", MsgAuth, msgType)
	}
	if len(payload) == 0 {
		t.Fatal("expected non-empty payload")
	}
}

func TestMouseAndKeyEncoding(t *testing.T) {
	m := MouseEvent{Action: MouseWheel, Button: ButtonRight, X: 32768, Y: 65535, Delta: -120}
	m2, err := DecodeMouse(EncodeMouse(m))
	if err != nil {
		t.Fatalf("DecodeMouse failed: %v", err)
	}
	if m != m2 {
		t.Fatalf("mouse mismatch: got %+v, want %+v", m2, m)
	}

	k := KeyEvent{Action: KeyDown, VK: 0x41, Scan: 0x1E, Flags: KeyFlagExtended}
	k2, err := DecodeKey(EncodeKey(k))
	if err != nil {
		t.Fatalf("DecodeKey failed: %v", err)
	}
	if k != k2 {
		t.Fatalf("key mismatch: got %+v, want %+v", k2, k)
	}
}
