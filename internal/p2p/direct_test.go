package p2p

import (
	"context"
	"testing"
	"time"

	"remote-control/internal/protocol"
)

func TestP2PDirectHandshake(t *testing.T) {
	ln, err := StartP2PListener()
	if err != nil {
		t.Fatalf("StartP2PListener failed: %v", err)
	}
	defer ln.Close()

	eps := ln.Endpoints()
	if len(eps) == 0 {
		t.Fatal("expected at least 1 endpoint")
	}

	offer := protocol.P2POfferPayload{
		SessionID: "sess-test-1",
		Endpoints: eps,
		Token:     ln.Token(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	adminConn, err := DialP2POffer(ctx, offer, 1*time.Second)
	if err != nil {
		t.Fatalf("DialP2POffer failed: %v", err)
	}
	defer adminConn.Close()

	select {
	case clientConn := <-ln.Conn():
		defer clientConn.Close()
		// Test bidirectional data flow over direct TLS 1.3 socket.
		testMsg := []byte("hello-p2p-direct")
		if err := protocol.WriteMessage(adminConn, protocol.MsgPing, testMsg); err != nil {
			t.Fatalf("admin write failed: %v", err)
		}
		mType, data, err := protocol.ReadMessage(clientConn)
		if err != nil || mType != protocol.MsgPing || string(data) != string(testMsg) {
			t.Fatalf("client read mismatch: got %v err=%v", string(data), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client direct conn")
	}
}

func TestP2PDirectRejectsBadToken(t *testing.T) {
	ln, err := StartP2PListener()
	if err != nil {
		t.Fatalf("StartP2PListener failed: %v", err)
	}
	defer ln.Close()

	offer := protocol.P2POfferPayload{
		SessionID: "sess-test-2",
		Endpoints: ln.Endpoints(),
		Token:     "wrong-bad-token",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err = DialP2POffer(ctx, offer, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected bad token to be rejected")
	}
}
