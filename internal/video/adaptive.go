package video

import (
	"sync"
	"time"
)

const (
	MinQuality      = 40
	MaxQuality      = 88
	DefaultQuality  = 78
	MinInterval     = 16 * time.Millisecond // ~60 FPS
	MaxInterval     = 66 * time.Millisecond // ~15 FPS
	DefaultInterval = 25 * time.Millisecond // ~40 FPS
)

// AdaptiveController dynamically scales video encoding quality (JPEG compression factor)
// and frame interval (FPS) based on real-time round-trip latency (RTT) and frame ACK backpressure,
// matching RustDesk's bandwidth-adaptive streaming engine.
type AdaptiveController struct {
	mu           sync.Mutex
	isAuto       bool
	quality      int
	interval     time.Duration
	emaRTT       float64
	sentTimes    map[uint32]time.Time
	consecFast   int
	consecSlow   int
}

// NewAdaptiveController creates a new AdaptiveController initialized in automatic mode.
func NewAdaptiveController() *AdaptiveController {
	return &AdaptiveController{
		isAuto:     true,
		quality:    DefaultQuality,
		interval:   DefaultInterval,
		emaRTT:     30.0,
		sentTimes:  make(map[uint32]time.Time, 16),
	}
}

// SetAuto enables or disables dynamic latency-driven adaptation.
func (c *AdaptiveController) SetAuto(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.isAuto = enabled
}

// SetQuality manually overrides the encoding quality and turns off auto mode until re-enabled.
func (c *AdaptiveController) SetQuality(q int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q < 30 {
		q = 30
	} else if q > 95 {
		q = 95
	}
	c.quality = q
	c.isAuto = false
}

// Quality returns the current JPEG compression factor.
func (c *AdaptiveController) Quality() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quality
}

// Interval returns the current target duration between screen frames.
func (c *AdaptiveController) Interval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval
}

// EMARtt returns the smoothed exponential moving average RTT in milliseconds.
func (c *AdaptiveController) EMARtt() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.emaRTT
}

// RecordSent registers a frame ID and its dispatch timestamp.
func (c *AdaptiveController) RecordSent(frameID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentTimes[frameID] = time.Now()
	// Prune old entries to prevent memory leak if ACKs are dropped.
	if len(c.sentTimes) > 64 {
		cutoff := time.Now().Add(-5 * time.Second)
		for id, t := range c.sentTimes {
			if t.Before(cutoff) {
				delete(c.sentTimes, id)
			}
		}
	}
}

// RecordAck processes an incoming MsgFrameAck from the viewer.
// inFlight is the current number of unacknowledged frames (lastSent - lastAcked).
func (c *AdaptiveController) RecordAck(frameID uint32, inFlight int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sent, ok := c.sentTimes[frameID]
	if ok {
		delete(c.sentTimes, frameID)
		rtt := float64(time.Since(sent).Milliseconds())
		if rtt < 1.0 {
			rtt = 1.0
		}
		c.emaRTT = 0.85*c.emaRTT + 0.15*rtt
	}

	if !c.isAuto {
		return
	}

	// Backpressure detection: viewer queue congested or network buffer bloated.
	if inFlight >= 2 || c.emaRTT > 140.0 {
		c.consecSlow++
		c.consecFast = 0
		if c.consecSlow >= 1 {
			c.quality -= 5
			if c.quality < MinQuality {
				c.quality = MinQuality
			}
			c.interval += 6 * time.Millisecond
			if c.interval > MaxInterval {
				c.interval = MaxInterval
			}
			c.consecSlow = 0
		}
		return
	}

	// Smooth link: low latency and zero queue backpressure.
	if inFlight <= 1 && c.emaRTT < 55.0 {
		c.consecSlow = 0
		c.consecFast++
		if c.consecFast >= 3 {
			c.quality += 2
			if c.quality > MaxQuality {
				c.quality = MaxQuality
			}
			c.interval -= 2 * time.Millisecond
			if c.interval < MinInterval {
				c.interval = MinInterval
			}
			c.consecFast = 0
		}
	}
}
