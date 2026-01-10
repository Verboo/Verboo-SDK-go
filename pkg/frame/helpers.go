// helpers package functions provide safe helpers for copying payloads and
// cloning frames when long-lived ownership is required. Use CopyPayload and
// CloneFrameForStorage whenever you will persist, store in maps, or send a
// payload to background goroutines.

package frame

const RS byte = 0x1E // Record Separator used between JSON header and body in payloads

// CopyPayload returns an independent copy of src (or nil for empty input).
// The returned slice is owned by the caller and is safe to store or mutate.
func CopyPayload(src []byte) []byte {
	if len(src) == 0 { // Fast-path: return nil for empty input to avoid allocating zero-length slices.
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst // Return independent slice owned by caller
}

// CloneFrameForStorage performs a deep copy of f suitable for long-lived
// storage or retry logic. The returned Frame owns its payload slice and does
// not reference pooled buffers.
func CloneFrameForStorage(f *Frame) *Frame {
	if f == nil { // Guard against nil input
		return nil
	}
	nf := &Frame{
		Type:     f.Type,
		Flags:    f.Flags,
		Version:  f.Version,
		StreamID: f.StreamID,
	}
	if len(f.Payload) > 0 {
		nf.Payload = CopyPayload(f.Payload) // Create independent copy of payload
	}
	return nf // Return deep-copied frame owned by caller
}

// BuildPooledPublishBufferWithPrefix builds a pooled encoded frame buffer
// whose payload is headerJSON + RS + body. It returns a pooled []byte which
// the caller MUST ReleaseEncoded exactly once after use.
func BuildPooledPublishBufferWithPrefix(headerJSON []byte, body []byte) ([]byte, error) {
	// Build combined payload (copy headerJSON to avoid referencing caller buffer)
	sep := byte(RS)
	combined := make([]byte, 0, len(headerJSON)+1+len(body))
	combined = append(combined, headerJSON...)
	combined = append(combined, sep)
	combined = append(combined, body...)

	f := &Frame{
		Type:     FrameRoomMessage,
		Flags:    0,
		Version:  1,
		StreamID: 0,
		Payload:  combined,
	}
	return EncodePooled(f) // Returns pooled buffer owned by caller
}
