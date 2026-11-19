//go:build windows

package win

import (
	"testing"
	"unsafe"
)

func TestDXGIComStructLayouts(t *testing.T) {
	if got := unsafe.Sizeof(guid{}); got != 16 {
		t.Fatalf("GUID size = %d, want 16", got)
	}

	wantDesc := uintptr(96)
	wantMapped := uintptr(16)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		wantDesc = 92
		wantMapped = 12
	}

	if got := unsafe.Sizeof(dxgiOutputDesc{}); got != wantDesc {
		t.Fatalf("DXGI_OUTPUT_DESC size = %d, want %d", got, wantDesc)
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
	if got := unsafe.Sizeof(d3d11MappedSubresource{}); got != wantMapped {
		t.Fatalf("D3D11_MAPPED_SUBRESOURCE size = %d, want %d", got, wantMapped)
	}
	if got := unsafe.Sizeof(d3d11Box{}); got != 24 {
		t.Fatalf("D3D11_BOX size = %d, want 24", got)
	}
}
