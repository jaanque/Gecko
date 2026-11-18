package config

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestTrailerAppendAndRead(t *testing.T) {
	base := []byte("MZ\x90\x00fake-pe-binary-payload-1234567890")
	logo := makeTestLogoPNG(t, 256, 128)
	normLogo, err := NormalizeLogoBytes(logo)
	if err != nil {
		t.Fatalf("NormalizeLogoBytes failed: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(normLogo))
	if err != nil {
		t.Fatalf("normalized logo is not valid PNG: %v", err)
	}
	if decoded.Bounds().Dx() != 128 || decoded.Bounds().Dy() != 64 {
		t.Fatalf("expected resized 128x64, got %dx%d", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}

	cfg1 := ClientConfig{
		ServerAddr:    "192.168.1.50:9443",
		CertSHA256:    strings.Repeat("a", 64),
		AuthToken:     "0123456789abcdef0123456789abcdef",
		ClientCertDER: []byte("cert-der-1"),
		ClientKeyDER:  []byte("key-der-1"),
		LogoPNG:       normLogo,
	}

	built, err := AppendToBinary(base, cfg1)
	if err != nil {
		t.Fatalf("AppendToBinary failed: %v", err)
	}

	got, err := ReadFromBytes(built)
	if err != nil {
		t.Fatalf("ReadFromBytes failed: %v", err)
	}
	if got.ServerAddr != cfg1.ServerAddr || got.CertSHA256 != cfg1.CertSHA256 || got.AuthToken != cfg1.AuthToken ||
		!bytes.Equal(got.ClientCertDER, cfg1.ClientCertDER) || !bytes.Equal(got.ClientKeyDER, cfg1.ClientKeyDER) ||
		!bytes.Equal(got.LogoPNG, cfg1.LogoPNG) {
		t.Fatalf("mismatch: got %+v, want %+v", got, cfg1)
	}

	// Re-embedding should replace the previous trailer cleanly.
	cfg2 := ClientConfig{
		ServerAddr:    "10.0.0.5:9443",
		CertSHA256:    strings.Repeat("b", 64),
		AuthToken:     "fedcba9876543210fedcba9876543210",
		ClientCertDER: []byte("cert-der-2"),
		ClientKeyDER:  []byte("key-der-2"),
	}
	rebuilt, err := AppendToBinary(built, cfg2)
	if err != nil {
		t.Fatalf("second AppendToBinary failed: %v", err)
	}
	got2, err := ReadFromBytes(rebuilt)
	if err != nil {
		t.Fatalf("ReadFromBytes(rebuilt) failed: %v", err)
	}
	if got2.ServerAddr != cfg2.ServerAddr || got2.CertSHA256 != cfg2.CertSHA256 || got2.AuthToken != cfg2.AuthToken || len(got2.LogoPNG) != 0 {
		t.Fatalf("rebuilt mismatch: got %+v, want %+v", got2, cfg2)
	}
}

func makeTestLogoPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 30, G: 90, B: 180, A: 200})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode failed: %v", err)
	}
	return buf.Bytes()
}
