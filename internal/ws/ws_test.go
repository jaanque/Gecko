package ws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSocketHandshakeAndEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()

		op, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if op == OpText {
			_ = conn.WriteText(data)
		} else {
			_ = conn.WriteBinary(data)
		}
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	rawConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer rawConn.Close()

	key := "dGhlIHNhbXBsZSBub25jZQ=="
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := rawConn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake failed: %v", err)
	}

	br := bufio.NewReader(rawConn)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 Switching Protocols, got %q (err=%v)", statusLine, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers failed: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Send masked binary payload.
	payload := []byte("hello-remote-ws")
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	var frame bytes.Buffer
	frame.WriteByte(0x80 | OpBinary)
	frame.WriteByte(0x80 | byte(len(payload)))
	frame.Write(mask[:])
	for i, b := range payload {
		frame.WriteByte(b ^ mask[i%4])
	}
	if _, err := rawConn.Write(frame.Bytes()); err != nil {
		t.Fatalf("write frame failed: %v", err)
	}

	// Read server unmasked echo frame.
	var hdr [2]byte
	if _, err := br.Read(hdr[:]); err != nil {
		t.Fatalf("read hdr failed: %v", err)
	}
	if hdr[0] != (0x80 | OpBinary) {
		t.Fatalf("unexpected opcode byte 0x%x", hdr[0])
	}
	n := int(hdr[1] & 0x7F)
	if n == 126 {
		var ext [2]byte
		_, _ = br.Read(ext[:])
		n = int(binary.BigEndian.Uint16(ext[:]))
	}
	got := make([]byte, n)
	if _, err := br.Read(got); err != nil {
		t.Fatalf("read payload failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q, want %q", got, payload)
	}
}
