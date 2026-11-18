package config

import (
	"strings"
	"testing"
)

func TestTrailerAppendAndRead(t *testing.T) {
	base := []byte("MZ\x90\x00fake-pe-binary-payload-1234567890")
	cfg1 := ClientConfig{
		ServerAddr: "192.168.1.50:9443",
		CertSHA256: strings.Repeat("a", 64),
		AuthToken:  "0123456789abcdef0123456789abcdef",
	}

	built, err := AppendToBinary(base, cfg1)
	if err != nil {
		t.Fatalf("AppendToBinary failed: %v", err)
	}

	got, err := ReadFromBytes(built)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if got != cfg1 {
		t.Fatalf("mismatch: got %+v, want %+v", got, cfg1)
	}

	// Re-embedding should replace the previous trailer cleanly without growing unboundedly.
	cfg2 := ClientConfig{
		ServerAddr: "10.0.0.5:9443",
		CertSHA256: strings.Repeat("b", 64),
		AuthToken:  "fedcba9876543210fedcba9876543210",
	}
	rebuilt, err := AppendToBinary(built, cfg2)
	if err != nil {
		t.Fatalf("second AppendToBinary failed: %v", err)
	}
	got2, err := ReadFromBytes(rebuilt)
	if err != nil {
		t.Fatalf("ReadFromBytes(rebuilt) failed: %v", err)
	}
	if got2 != cfg2 {
		t.Fatalf("rebuilt mismatch: got %+v, want %+v", got2, cfg2)
	}
	if len(rebuilt) > len(built)+16 {
		t.Fatalf("expected trailer replacement, size grew unexpectedly from %d to %d", len(built), len(rebuilt))
	}
}
