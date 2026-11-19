//go:build windows

package win

import "testing"

func TestSwitchThreadToInputDesktop(t *testing.T) {
	// Should execute cleanly without panic in unit tests.
	SwitchThreadToInputDesktop()
}
