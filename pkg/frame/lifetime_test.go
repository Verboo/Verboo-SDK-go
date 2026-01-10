// Package frame implements a compact binary frame protocol with helpers for
// zero-copy decoding, pooled encoding, and small-header encoding into existing
// buffers. Callers must observe ownership contracts: DecodeInto and
// ZeroCopyDecode return payload slices that reference the input buffer and
// must be copied before long-term storage; EncodePooled returns a pooled
// []byte that the caller MUST return with ReleaseEncoded exactly once.
package frame

import (
	"bytes"
	"testing"
)

// TestZeroCopyVsClone ensures that ZeroCopyDecode returns a payload view into input buffer
// and that CloneFrameForStorage produces an independent copy.
func TestZeroCopyVsClone(t *testing.T) {
	orig := &Frame{
		Type:     FrameData,
		Flags:    0,
		Version:  1,
		StreamID: 42,
		Payload:  []byte("zero-copy-payload"),
	}
	buf, err := Encode(orig)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// ZeroCopyDecode should produce Frame.Payload referencing buf
	fz, err := ZeroCopyDecode(buf)
	if err != nil {
		t.Fatalf("ZeroCopyDecode failed: %v", err)
	}
	if len(fz.Payload) == 0 {
		t.Fatalf("expected non-empty payload")
	}
	// mutate underlying buffer and ensure payload sees it
	buf[12] = '^' // first byte of payload area
	if fz.Payload[0] != '^' {
		t.Fatalf("zero-copy expectation failed: payload not referencing original buffer")
	}

	// Now test CloneFrameForStorage produces an independent copy
	fc := CloneFrameForStorage(fz)
	if fc == nil {
		t.Fatalf("CloneFrameForStorage returned nil")
	}
	// mutate original buf again - cloned payload must not change
	buf[12] = 'X'
	if fc.Payload[0] == 'X' {
		t.Fatalf("CloneFrameForStorage payload shares memory with original buffer")
	}

	// Also verify CopyPayload returns independent slice
	cp := CopyPayload(fz.Payload)
	if !bytes.Equal(cp, fz.Payload) {
		t.Fatalf("CopyPayload did not produce same content")
	}
	cp[0] = 'Z'
	if fz.Payload[0] == 'Z' {
		t.Fatalf("CopyPayload returned slice that aliases original payload")
	}
}
