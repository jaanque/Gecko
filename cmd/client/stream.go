//go:build windows

package main

import (
	"context"
	"net"
	"time"

	"remote-control/internal/protocol"
	"remote-control/internal/win"
)

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (a *clientAgent) streamScreen(ctx context.Context, conn net.Conn, sid string) {
	capturer := win.NewScreenCapturer()
	defer capturer.Close()

	a.lastSentFrameID.Store(0)
	a.lastAckedFrameID.Store(0)

	const (
		frameInterval = 16 * time.Millisecond
		maxInFlight   = 2
	)

	timer := time.NewTimer(frameInterval)
	defer timer.Stop()

	sleep := func(d time.Duration) bool {
		resetTimer(timer, d)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}

	var frameSeq uint32

	for ctx.Err() == nil {
		if int32(a.lastSentFrameID.Load()-a.lastAckedFrameID.Load()) >= maxInFlight {
			resetTimer(timer, 300*time.Millisecond)
			select {
			case <-ctx.Done():
				return
			case <-a.ackWakeCh:
			case <-timer.C:
				a.lastAckedFrameID.Store(a.lastSentFrameID.Load())
			}
		}

		start := time.Now()
		q := int(a.quality.Load())
		monIdx := int(a.monitorIdx.Load())
		force := a.forceKeyframe.Swap(false)

		nextID := frameSeq + 1
		frame, _, _, err := capturer.CaptureFrame(nextID, monIdx, q, force)
		if err != nil {
			if !sleep(200 * time.Millisecond) {
				return
			}
			continue
		}

		if len(frame) > 0 {
			frameSeq = nextID
			a.lastSentFrameID.Store(frameSeq)
			if err := a.writeMessage(conn, protocol.MsgFrame, frame); err != nil {
				a.handleRemoteSessionStop(sid)
				return
			}
		}

		wait := max(frameInterval-time.Since(start), 2*time.Millisecond)
		if !sleep(wait) {
			return
		}
	}
}
