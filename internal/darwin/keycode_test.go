package darwin

import "testing"

func TestVKToMacKeycode(t *testing.T) {
	if kc, ok := VKToMacKeycode(0x41); !ok || kc != 0 {
		t.Fatalf("VK_A -> (%d, %v), want (0, true)", kc, ok)
	}
	if kc, ok := VKToMacKeycode(0x0D); !ok || kc != 36 {
		t.Fatalf("VK_RETURN -> (%d, %v), want (36, true)", kc, ok)
	}
	if kc, ok := VKToMacKeycode(0x25); !ok || kc != 123 {
		t.Fatalf("VK_LEFT -> (%d, %v), want (123, true)", kc, ok)
	}
}
