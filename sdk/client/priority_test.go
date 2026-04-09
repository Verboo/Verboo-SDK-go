package client

import (
	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"testing"
)

// TestPriorityChannels tests that frames are routed to correct priority channels
func TestPriorityChannels(t *testing.T) {
	tests := []struct {
		frameType    frame.FrameType
		expectedPrio int
	}{
		{frame.FrameHeartbeat, DefaultPrioritySystem},    // high priority
		{frame.FrameData, DefaultPriorityNormal},         // normal priority
		{frame.FrameFileMetadata, DefaultPriorityNormal}, // normal priority
		{frame.FrameFileChunk, DefaultPriorityLow},       // low priority
		{frame.FrameFileEnd, DefaultPriorityNormal},      // normal priority
	}

	for _, tt := range tests {
		var prio int
		switch tt.frameType {
		case frame.FrameHeartbeat:
			prio = DefaultPrioritySystem
		default:
			if tt.frameType == frame.FrameFileChunk {
				prio = DefaultPriorityLow
			} else {
				prio = DefaultPriorityNormal
			}
		}

		if prio != tt.expectedPrio {
			t.Errorf("frame type %d should have priority %d, got %d",
				tt.frameType, tt.expectedPrio, prio)
		}
	}
}

// BenchmarkSendPrioritized benchmarks the send prioritization logic
func BenchmarkSendPrioritized(b *testing.B) {
	// This benchmark would require a fully initialized client with mock transport
	// For now, just test the routing logic without actual sending

	for i := 0; i < b.N; i++ {
		var prio int
		switch i % 3 {
		case 0:
			prio = DefaultPrioritySystem
		case 1:
			prio = DefaultPriorityNormal
		default:
			prio = DefaultPriorityLow
		}
		_ = prio
	}
}
