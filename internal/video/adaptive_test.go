package video

import (
	"testing"
	"time"
)

func TestAdaptiveControllerInitialState(t *testing.T) {
	ac := NewAdaptiveController()
	if q := ac.Quality(); q != DefaultQuality {
		t.Fatalf("initial quality = %d, want %d", q, DefaultQuality)
	}
	if iv := ac.Interval(); iv != DefaultInterval {
		t.Fatalf("initial interval = %v, want %v", iv, DefaultInterval)
	}
}

func TestAdaptiveControllerBackpressureThrottle(t *testing.T) {
	ac := NewAdaptiveController()
	initQ := ac.Quality()
	initIv := ac.Interval()

	// Simulate high backpressure (inFlight = 3).
	ac.RecordAck(10, 3)

	if ac.Quality() >= initQ {
		t.Fatalf("expected quality to drop under backpressure: got %d, initial %d", ac.Quality(), initQ)
	}
	if ac.Interval() <= initIv {
		t.Fatalf("expected interval to increase under backpressure: got %v, initial %v", ac.Interval(), initIv)
	}
}

func TestAdaptiveControllerSmoothLinkRampUp(t *testing.T) {
	ac := NewAdaptiveController()
	ac.SetQuality(MinQuality)
	ac.SetAuto(true)

	// Simulate consecutive fast frames.
	for i := uint32(1); i <= 6; i++ {
		ac.RecordSent(i)
		time.Sleep(2 * time.Millisecond)
		ac.RecordAck(i, 0)
	}

	if ac.Quality() <= MinQuality {
		t.Fatalf("expected quality to ramp up on fast link: got %d", ac.Quality())
	}
}

func TestAdaptiveControllerManualOverride(t *testing.T) {
	ac := NewAdaptiveController()
	ac.SetQuality(85)
	if ac.Quality() != 85 {
		t.Fatalf("expected quality 85, got %d", ac.Quality())
	}

	// Should not auto-throttle when manual.
	ac.RecordAck(1, 4)
	if ac.Quality() != 85 {
		t.Fatalf("manual quality altered by ACK: got %d, want 85", ac.Quality())
	}
}
