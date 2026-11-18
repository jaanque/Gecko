package linux

// VKToKeySym maps a Windows Virtual-Key code (VK_*) to an X11 KeySym (XK_*).
// Returns 0 if no standard KeySym mapping exists.
func VKToKeySym(vk uint16) uint32 {
	// Letters A-Z -> lowercase ASCII keysyms (Shift modifier produces uppercase).
	if vk >= 0x41 && vk <= 0x5A {
		return uint32(vk - 0x41 + 'a')
	}
	// Top-row digits 0-9.
	if vk >= 0x30 && vk <= 0x39 {
		return uint32(vk)
	}
	// Function keys F1-F24.
	if vk >= 0x70 && vk <= 0x87 {
		return 0xFFBE + uint32(vk-0x70)
	}
	// Numpad 0-9.
	if vk >= 0x60 && vk <= 0x69 {
		return 0xFFB0 + uint32(vk-0x60)
	}

	switch vk {
	case 0x08: // VK_BACK
		return 0xFF08
	case 0x09: // VK_TAB
		return 0xFF09
	case 0x0D: // VK_RETURN
		return 0xFF0D
	case 0x10, 0xA0: // VK_SHIFT, VK_LSHIFT
		return 0xFFE1
	case 0xA1: // VK_RSHIFT
		return 0xFFE2
	case 0x11, 0xA2: // VK_CONTROL, VK_LCONTROL
		return 0xFFE3
	case 0xA3: // VK_RCONTROL
		return 0xFFE4
	case 0x12, 0xA4: // VK_MENU, VK_LMENU (Alt)
		return 0xFFE9
	case 0xA5: // VK_RMENU (AltGr)
		return 0xFE03 // ISO_Level3_Shift
	case 0x13: // VK_PAUSE
		return 0xFF13
	case 0x14: // VK_CAPITAL (CapsLock)
		return 0xFFE5
	case 0x1B: // VK_ESCAPE
		return 0xFF1B
	case 0x20: // VK_SPACE
		return 0x0020
	case 0x21: // VK_PRIOR (PageUp)
		return 0xFF55
	case 0x22: // VK_NEXT (PageDown)
		return 0xFF56
	case 0x23: // VK_END
		return 0xFF57
	case 0x24: // VK_HOME
		return 0xFF50
	case 0x25: // VK_LEFT
		return 0xFF51
	case 0x26: // VK_UP
		return 0xFF52
	case 0x27: // VK_RIGHT
		return 0xFF53
	case 0x28: // VK_DOWN
		return 0xFF54
	case 0x2C: // VK_SNAPSHOT (Print)
		return 0xFF61
	case 0x2D: // VK_INSERT
		return 0xFF63
	case 0x2E: // VK_DELETE
		return 0xFFFF
	case 0x5B: // VK_LWIN (Super_L)
		return 0xFFEB
	case 0x5C: // VK_RWIN (Super_R)
		return 0xFFEC
	case 0x5D: // VK_APPS (Menu)
		return 0xFF67
	case 0x6A: // VK_MULTIPLY
		return 0xFFAA
	case 0x6B: // VK_ADD
		return 0xFFAB
	case 0x6D: // VK_SUBTRACT
		return 0xFFAD
	case 0x6E: // VK_DECIMAL
		return 0xFFAE
	case 0x6F: // VK_DIVIDE
		return 0xFFAF
	case 0x90: // VK_NUMLOCK
		return 0xFF7F
	case 0x91: // VK_SCROLL
		return 0xFF14
	case 0xBA: // VK_OEM_1 (;)
		return 0x003B
	case 0xBB: // VK_OEM_PLUS (=)
		return 0x003D
	case 0xBC: // VK_OEM_COMMA (,)
		return 0x002C
	case 0xBD: // VK_OEM_MINUS (-)
		return 0x002D
	case 0xBE: // VK_OEM_PERIOD (.)
		return 0x002E
	case 0xBF: // VK_OEM_2 (/)
		return 0x002F
	case 0xC0: // VK_OEM_3 (`)
		return 0x0060
	case 0xDB: // VK_OEM_4 ([)
		return 0x005B
	case 0xDC: // VK_OEM_5 (\)
		return 0x005C
	case 0xDD: // VK_OEM_6 (])
		return 0x005D
	case 0xDE: // VK_OEM_7 (')
		return 0x0027
	case 0xE2: // VK_OEM_102 (< >)
		return 0x003C
	}
	return 0
}

// ScanToEvdevKeycode converts a standard Windows PS/2 Set-1 scancode (plus extended bit)
// to an X11 evdev keycode (evdev + 8), used as a layout-preserving fallback for regional keys.
func ScanToEvdevKeycode(scan uint16, extended bool) byte {
	if scan == 0 || scan > 0x7F {
		return 0
	}
	if !extended {
		// On Xorg with evdev (virtually all modern Linux desktops), non-extended Set-1 scancodes
		// 0x01..0x58 map directly to X11 keycodes scancode+8.
		if scan >= 0x01 && scan <= 0x58 {
			return byte(scan + 8)
		}
		return 0
	}
	// Common extended Set-1 scancodes -> X11 evdev keycodes.
	switch scan {
	case 0x1C: // KP_Enter
		return 104
	case 0x1D: // Control_R
		return 105
	case 0x35: // KP_Divide
		return 106
	case 0x38: // AltGr (ISO_Level3_Shift)
		return 108
	case 0x47: // Home
		return 110
	case 0x48: // Up
		return 111
	case 0x49: // PageUp
		return 112
	case 0x4B: // Left
		return 113
	case 0x4D: // Right
		return 114
	case 0x4F: // End
		return 115
	case 0x50: // Down
		return 116
	case 0x51: // PageDown
		return 117
	case 0x52: // Insert
		return 118
	case 0x53: // Delete
		return 119
	case 0x5B: // Super_L
		return 133
	case 0x5C: // Super_R
		return 134
	case 0x5D: // Menu
		return 135
	}
	return 0
}
