package frame

import (
	"sync"
	"testing"
)

func TestEncodePooledConcurrency(t *testing.T) {
	f := &Frame{Type: FrameData, Flags: 0, Version: 1, StreamID: 1, Payload: []byte("payload-data")}

	const goroutines = 50
	const iters = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				b, err := EncodePooled(f)
				if err != nil {
					errCh <- err
					return
				}
				// Basic sanity checks
				if len(b) < HeaderLen {
					errCh <- err
					return
				}
				ReleaseEncoded(b)
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatalf("concurrent EncodePooled failed: %v", e)
		}
	}
}
