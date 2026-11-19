package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"remote-control/internal/protocol"
)

func TestReceiverSuccessAndUniqueNaming(t *testing.T) {
	dir := t.TempDir()
	rec := NewReceiver(dir)
	defer rec.AbortAll()

	payload := []byte("hola-control-remoto-archivo-seguro")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	// 1. Send with traversal attempt in filename -> sanitized into dir.
	if err := rec.StartOffer(protocol.FileOfferPayload{
		TransferID: "tx1",
		Name:       "../../malicioso/informe.txt",
		Size:       int64(len(payload)),
		SHA256:     sumHex,
	}); err != nil {
		t.Fatalf("StartOffer failed: %v", err)
	}
	if err := rec.WriteChunk(protocol.FileChunk{TransferID: "tx1", Offset: 0, Data: payload[:10]}); err != nil {
		t.Fatalf("WriteChunk 1 failed: %v", err)
	}
	if err := rec.WriteChunk(protocol.FileChunk{TransferID: "tx1", Offset: 10, Data: payload[10:]}); err != nil {
		t.Fatalf("WriteChunk 2 failed: %v", err)
	}
	saved1, err := rec.Finish("tx1")
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if filepath.Dir(saved1) != dir || filepath.Base(saved1) != "informe.txt" {
		t.Fatalf("unexpected saved path: %q", saved1)
	}
	gotBytes, err := os.ReadFile(saved1)
	if err != nil || string(gotBytes) != string(payload) {
		t.Fatalf("saved content mismatch: %q err=%v", string(gotBytes), err)
	}

	// 2. Sending the same filename again must NOT overwrite existing file.
	if err := rec.StartOffer(protocol.FileOfferPayload{
		TransferID: "tx2",
		Name:       "informe.txt",
		Size:       int64(len(payload)),
		SHA256:     sumHex,
	}); err != nil {
		t.Fatalf("StartOffer 2 failed: %v", err)
	}
	if err := rec.WriteChunk(protocol.FileChunk{TransferID: "tx2", Offset: 0, Data: payload}); err != nil {
		t.Fatalf("WriteChunk failed: %v", err)
	}
	saved2, err := rec.Finish("tx2")
	if err != nil {
		t.Fatalf("Finish 2 failed: %v", err)
	}
	if saved2 == saved1 || filepath.Base(saved2) != "informe (1).txt" {
		t.Fatalf("expected non-colliding filename informe (1).txt, got %q", saved2)
	}
}

func TestReceiverRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	rec := NewReceiver(dir)
	defer rec.AbortAll()

	badSum := sha256.Sum256([]byte("other"))
	if err := rec.StartOffer(protocol.FileOfferPayload{
		TransferID: "tx-bad",
		Name:       "corrupto.bin",
		Size:       5,
		SHA256:     hex.EncodeToString(badSum[:]),
	}); err != nil {
		t.Fatalf("StartOffer failed: %v", err)
	}
	if err := rec.WriteChunk(protocol.FileChunk{TransferID: "tx-bad", Offset: 0, Data: []byte("12345")}); err != nil {
		t.Fatalf("WriteChunk failed: %v", err)
	}
	if _, err := rec.Finish("tx-bad"); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	// Temp file must have been cleaned up.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected temp file cleanup, found %d entries", len(entries))
	}
}
