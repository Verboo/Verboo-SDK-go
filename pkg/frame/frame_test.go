//go:build bench
// +build bench

package frame

import (
	"bytes"
	"testing"
)

// TestEncodeDecode verifies that Encode and Decode are lossless for a variety of payload sizes.
func TestEncodeDecode(t *testing.T) {
	cases := [][]byte{
		[]byte{},
		[]byte("small payload"),
		bytes.Repeat([]byte{0x55}, 1024),    // 1 KiB
		bytes.Repeat([]byte{0xAA}, 64*1024), // 64 KiB
	}
	for i, payload := range cases {
		in := &Frame{Type: FrameData, Flags: 0x1, Version: 1, StreamID: uint32(i + 1), Payload: payload}
		buf, err := Encode(in)
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		out, err := Decode(buf)
		if err != nil {
			t.Fatalf("Decode failed: %v", err)
		}
		if out.Type != in.Type || out.Flags != in.Flags || out.Version != in.Version || out.StreamID != in.StreamID {
			t.Fatalf("header mismatch: got %+v want %+v", out, in)
		}
		if !bytes.Equal(out.Payload, in.Payload) {
			t.Fatalf("payload mismatch for case %d: len got=%d want=%d", i, len(out.Payload), len(in.Payload))
		}
	}
}

// TestZeroCopyDecode ensures ZeroCopyDecode returns a payload slice that references the input buffer
// and that modifying the input buffer affects the frame.Payload (demonstrates zero-copy semantics).
func TestZeroCopyDecode(t *testing.T) {
	orig := &Frame{Type: FrameData, Flags: 0x0, Version: 1, StreamID: 42, Payload: []byte("hello-zero-copy")}
	buf, err := Encode(orig)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	f, err := ZeroCopyDecode(buf)
	if err != nil {
		t.Fatalf("ZeroCopyDecode: %v", err)
	}
	// f.Payload should be a subslice of buf; changing buf's payload area should reflect in f.Payload.
	if len(f.Payload) == 0 {
		t.Fatalf("expected non-empty payload")
	}
	// mutate underlying buffer where payload lives
	buf[12] = '^' // first byte of payload
	if f.Payload[0] != '^' {
		t.Fatalf("zero-copy expectation failed: payload not referencing input buffer")
	}
}

// TestEncodePooledAndRelease checks that EncodePooled returns a buffer and ReleaseEncoded accepts it.
func TestEncodePooledAndRelease(t *testing.T) {
	in := &Frame{Type: FrameData, Flags: 0x0, Version: 1, StreamID: 7, Payload: []byte("pooled")}
	b, err := EncodePooled(in)
	if err != nil {
		t.Fatalf("EncodePooled: %v", err)
	}
	// basic sanity: buffer should contain header + payload length
	if len(b) < HeaderLen {
		t.Fatalf("pooled buffer too small")
	}
	// ReleaseEncoded should not panic and should allow reuse
	ReleaseEncoded(b)
	// get another buffer and ensure pool didn't break
	b2, err := EncodePooled(in)
	if err != nil {
		t.Fatalf("second EncodePooled: %v", err)
	}
	ReleaseEncoded(b2)
}

// BenchmarkEncode measures Encode allocations and time.
func BenchmarkEncode(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x01}, 1500)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Encode(f)
	}
}

// BenchmarkDecodeIntoPooled measures BenchmarkDecodeIntoPooled performance.
func BenchmarkDecodeIntoPooled(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x02}, 1500)}
	buf, _ := Encode(f)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		pf := GetFrame()        // from pool, no alloc
		_ = DecodeInto(pf, buf) // zero-copy view into buf, no alloc
		PutFrame(pf)            // release: clear and return to pool
	}
}

// BenchmarkEncodePooled measures pooled encode performance and allocation reduction.
func BenchmarkEncodePooled(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x03}, 1500)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, _ := EncodePooled(f)
		ReleaseEncoded(buf)
	}
}

// BenchmarkEncodeInto measures encoding into a preallocated buffer to avoid allocs.
func BenchmarkEncodeInto(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x04}, 1500)}
	dst := make([]byte, 0, 4096)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// reuse dst capacity repeatedly; EncodeInto returns extended slice which we discard
		_, _ = EncodeInto(dst[:0], f)
	}
}
