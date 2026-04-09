package client

import (
	"sync"
	"time"
)

type clientRTTManager struct {
	mu          sync.Mutex
	smoothedRTT time.Duration
	pingTime    time.Time
}

func newClientRTTManager() *clientRTTManager {
	return &clientRTTManager{}
}

func (r *clientRTTManager) recordPing() {
	r.mu.Lock()
	r.pingTime = time.Now()
	r.mu.Unlock()
}

func (r *clientRTTManager) recordPong() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pingTime.IsZero() {
		return
	}
	rtt := time.Since(r.pingTime)
	if r.smoothedRTT == 0 {
		r.smoothedRTT = rtt
	} else {
		r.smoothedRTT = (7*r.smoothedRTT + rtt) / 8 // EWMA α=0.125
	}
	r.pingTime = time.Time{}
}

func (r *clientRTTManager) windowSize(chunkSize int) int {
	r.mu.Lock()
	rtt := r.smoothedRTT
	r.mu.Unlock()

	if rtt == 0 {
		return 64
	}

	window := int(10e6/8*rtt.Seconds()) / chunkSize
	if window < 32 {
		return 32
	}

	if window > 256 {
		return 256
	}
	if window > 256 {
		return 256
	}

	return window
}
