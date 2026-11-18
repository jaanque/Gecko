package linux

import "testing"

func TestVKToKeySym(t *testing.T) {
	cases := []struct {
		vk   uint16
		want uint32
	}{
		{0x41, 'a'},
		{0x5A, 'z'},
		{0x30, '0'},
		{0x39, '9'},
		{0x0D, 0xFF0D}, // Return
		{0x1B, 0xFF1B}, // Escape
		{0x20, 0x0020}, // Space
		{0x70, 0xFFBE}, // F1
		{0x7B, 0xFFC9}, // F12
		{0xA0, 0xFFE1}, // Shift_L
		{0xA2, 0xFFE3}, // Control_L
		{0xA4, 0xFFE9}, // Alt_L
		{0xA5, 0xFE03}, // AltGr
		{0x5B, 0xFFEB}, // Super_L
		{0x25, 0xFF51}, // Left
		{0x2E, 0xFFFF}, // Delete
	}
	for _, tc := range cases {
		if got := VKToKeySym(tc.vk); got != tc.want {
			t.Errorf("VKToKeySym(0x%X) = 0x%X, want 0x%X", tc.vk, got, tc.want)
		}
	}

	if kc := ScanToEvdevKeycode(0x1E, false); kc != 38 { // 'A' key
		t.Errorf("ScanToEvdevKeycode(0x1E, false) = %d, want 38", kc)
	}
	if kc := ScanToEvdevKeycode(0x48, true); kc != 111 { // Up arrow
		t.Errorf("ScanToEvdevKeycode(0x48, true) = %d, want 111", kc)
	}
}
