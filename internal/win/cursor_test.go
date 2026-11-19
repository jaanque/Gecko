//go:build windows

package win

import (
	"testing"
)

func TestCursorTrackerPoll(t *testing.T) {
	ct := NewCursorTracker()
	// Polling headlessly in tests should not panic and return valid state.
	_, _ = ct.Poll(0)
}
