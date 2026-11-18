//go:build windows && (amd64 || arm64)

package win

import (
	"testing"
	"unsafe"
)

func TestDXGIComStructLayouts(t *testing.T) {
	if got := unsafe.Sizeof(guid{}); got != 16 {
		t.Fatalf("GUID size = %d, want 16", got)
	}
	if got := unsafe.Sizeof(dxgiOutputDesc{}); got != 96 {
		t.Fatalf("DXGI_OUTPUT_DESC size = %d, want 96", got)
	}
	if got := unsafe.Sizeof(dxgiOutduplFrameInfo{}); got != 48 {
		t.Fatalf("DXGI_OUTDUPL_FRAME_INFO size = %d, want 48", got)
	}
	if got := unsafe.Sizeof(dxgiOutduplMoveRect{}); got != 24 {
		t.Fatalf("DXGI_OUTDUPL_MOVE_RECT size = %d, want 24", got)
	}
	if got := unsafe.Sizeof(d3d11Texture2DDesc{}); got != 44 {
		t.Fatalf("D3D11_TEXTURE2D_DESC size = %d, want 44", got)
	}
	if got := unsafe.Sizeof(d3d11MappedSubresource{}); got != 16 {
		t.Fatalf("D3D11_MAPPED_SUBRESOURCE size = %d, want 16", got)
	}
	if got := unsafe.Sizeof(d3d11Box{}); got != 24 {
		t.Fatalf("D3D11_BOX size = %d, want 24", got)
	}
}
