//go:build bench
// +build bench

package frame

import (
	"bytes"
	"testing"
)

// BenchmarkEncodePooled_Parallel measures EncodePooled performance under parallel load.
// Ensure ReleaseEncoded is called to avoid pool exhaustion during benchmarks.
func BenchmarkEncodePooled_Parallel(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x01}, 1500)}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf, err := EncodePooled(f)
			if err != nil {
				b.Fatalf("EncodePooled failed: %v", err)
			}
			ReleaseEncoded(buf) // Must release pooled buffer after use
		}
	})
}

// BenchmarkEncodeInto measures EncodeInto into a preallocated buffer in parallel.
func BenchmarkEncodeInto_Parallel(b *testing.B) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: bytes.Repeat([]byte{0x02}, 1500)}
	dst := make([]byte, 0, 8*1024) // Pre-allocated buffer
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = EncodeInto(dst[:0], f)
		}
	})
}
