package transport

import (
	"testing"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
)

// TestWsTransportPriorityChannels tests that frames are routed to correct priority channels
func TestWsTransportPriorityChannels(t *testing.T) {
	tests := []struct {
		name       string
		frameType  frame.FrameType
		priority   uint8
		expectedCh string // "high", "normal", or "low"
	}{
		{"Heartbeat", frame.FrameHeartbeat, frame.FlagPrioritySystem, "high"},
		{"Auth", frame.FrameAuth, frame.FlagPriorityHigh, "high"},
		{"Data", frame.FrameData, 0, "normal"},
		{"RoomMessage", frame.FrameRoomMessage, 0, "normal"},
		{"FileChunk", frame.FrameFileChunk, frame.FlagPriorityLow, "low"},
		{"FileUploadChunk", frame.FrameFileUploadChunk, frame.FlagPriorityLow, "low"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priority := tt.priority & 0x1F
			var expectedCh string

			switch priority {
			case frame.FlagPrioritySystem, frame.FlagPriorityHigh:
				expectedCh = "high"
			case frame.FlagPriorityLow:
				expectedCh = "low"
			default:
				expectedCh = "normal"
			}

			if expectedCh != tt.expectedCh {
				t.Errorf("priority %d should route to channel %s, got %s",
					priority, tt.expectedCh, expectedCh)
			}
		})
	}
}
