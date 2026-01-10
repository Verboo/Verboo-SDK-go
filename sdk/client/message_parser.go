package client

import (
	"encoding/json"
	"fmt"
	"github.com/Verboo/Verboo-SDK-go/internal/logger"
	"strings"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
)

// ParsedMessage contains the parsed message data from a frame.
type ParsedMessage struct {
	Header   frame.MessageHeader // Metadata extracted from header (may be empty with WithIgnoreHeader)
	Body     []byte              // Raw body content
	RawFrame *frame.Frame        // Reference to original frame for full access
}

// ParseOption configures how message parsing should behave.
type ParseOption func(*parseOptions)

// parseOptions holds configuration settings for message parser.
type parseOptions struct {
	fields       []string
	asString     bool
	ignoreHeader bool
}

// WithFilter includes only specified header fields in the parsed result.
func WithFilter(fields []string) ParseOption {
	return func(o *parseOptions) {
		o.fields = fields
	}
}

// WithBodyAsString converts body content to string instead of byte slice.
func WithBodyAsString() ParseOption {
	return func(o *parseOptions) {
		o.asString = true
	}
}

// WithIgnoreHeader returns only the body (header will be empty).
func WithIgnoreHeader() ParseOption {
	return func(o *parseOptions) {
		o.ignoreHeader = true
	}
}

// ParseMessage parses frame payload into structured message data.
func ParseMessage(f *frame.Frame, opts ...ParseOption) (*ParsedMessage, error) {
	// Validate frame type (only FrameData and FrameRoomMessage supported)
	if f.Type != frame.FrameData && f.Type != frame.FrameRoomMessage {
		return nil, fmt.Errorf("unsupported frame type: %d (only FrameData/FrameRoomMessage supported)", f.Type)
	}

	// Initialize options with default values
	options := &parseOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Process payload to extract header and body
	sep := byte(frame.RS) // Record Separator (0x1E)
	idx := -1

	// Locate separator in payload
	for i := 0; i < len(f.Payload); i++ {
		if f.Payload[i] == sep {
			idx = i
			break
		}
	}

	var header frame.MessageHeader
	body := []byte{}

	// If separator found, attempt to parse JSON header
	if idx >= 0 && idx > 0 {
		if err := json.Unmarshal(f.Payload[:idx], &header); err != nil {
			// Log warning and use entire payload as body on parsing failure
			logger.S().Warnw("failed to parse header", "err", err,
				"payload_prefix", string(f.Payload[:minimal(64, len(f.Payload))]))

			body = f.Payload // Entire payload as body
		} else {
			body = f.Payload[idx+1:] // Body after separator
		}
	} else {
		// No separator found - use entire payload as body
		logger.S().Warnw("missing RS separator, using entire payload as body",
			"payload_len", len(f.Payload))
		body = f.Payload
	}

	// Convert body to string if requested
	if options.asString {
		body = []byte(string(body))
	}

	// Apply header field filtering if specified
	if !options.ignoreHeader && len(options.fields) > 0 {
		filteredHeader := frame.MessageHeader{}
		for _, field := range options.fields {
			switch strings.ToLower(field) {
			case "message_id", "id":
				filteredHeader.MessageID = header.MessageID
			case "sequence":
				filteredHeader.Sequence = header.Sequence
			case "ts", "timestamp":
				filteredHeader.Timestamp = header.Timestamp
			case "from", "sender_id":
				filteredHeader.SenderID = header.SenderID
			case "to", "target_id":
				filteredHeader.TargetID = header.TargetID
			case "persistent":
				filteredHeader.Persistent = header.Persistent
			}
		}
		header = filteredHeader // Replace with filtered header
	} else if options.ignoreHeader {
		// Reset header to empty struct (not nil)
		header = frame.MessageHeader{}
	}

	return &ParsedMessage{
		Header:   header,
		Body:     body,
		RawFrame: f,
	}, nil
}
