package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/logger"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"

	"github.com/quic-go/quic-go/http3"
)

// Priority buffers for HTTP/3 transport.
// sendHighCh: heartbeats, control frames (critically important not to block)
// sendNormalCh: normal messages
// sendLowCh: file chunks (large buffer for backpressure)
const (
	HTTP3TransportSendHighBuffer   = 32  // for high priority frames
	HTTP3TransportSendNormalBuffer = 128 // for normal priority frames
	HTTP3TransportSendLowBuffer    = 512 // for low priority frames (file chunks)
)

// pendingHTTP3Send represents frame send with its type and data.
// Used only in HTTP/3 transport.
type pendingHTTP3Send struct {
	frameType frame.FrameType
	data      []byte
}

type HTTP3Transport struct {
	addr      string
	client    *http.Client      // Properly configured for HTTP/3 (quic-go)
	token     string            // JWT token for authentication
	recvCh    chan *frame.Frame // Channel to receive decoded frames
	ctx       context.Context
	cancel    context.CancelFunc
	closeMu   sync.Mutex
	closed    bool
	sseCancel context.CancelFunc // Context for SSE stream cancelation

	// Priority channels for sending frames (blocking SendFrame removed)
	sendHighCh   chan pendingHTTP3Send // HIGH priority: heartbeats, control frames
	sendNormalCh chan pendingHTTP3Send // NORMAL priority: messages
	sendLowCh    chan pendingHTTP3Send // LOW priority: file chunks

	// Backoff configuration for reconnect attempts
	backoff      time.Duration // Current backoff duration
	maxBackoff   time.Duration // Maximum backoff interval should be reasonable for production
	sendWorkerWg sync.WaitGroup

	// Single cb for OnFrame to prevent race conditions
	cb     func(*frame.Frame)
	recvMu sync.RWMutex
}

func NewHTTP3Transport(opts Options) (*HTTP3Transport, error) {
	if opts.Addr == "" {
		return nil, fmt.Errorf("address required for http3 transport")
	}
	if opts.Token == "" {
		return nil, fmt.Errorf("token required for http3 transport")
	}

	ctx, cancel := context.WithCancel(context.Background())

	tlsCfg := &tls.Config{
		InsecureSkipVerify: opts.Insecure,
	}

	httpClient := &http.Client{
		Transport: &http3.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	h := &HTTP3Transport{
		addr:         opts.Addr,
		client:       httpClient,
		token:        opts.Token,
		recvCh:       make(chan *frame.Frame, 128),
		ctx:          ctx,
		cancel:       cancel,
		sendHighCh:   make(chan pendingHTTP3Send, HTTP3TransportSendHighBuffer),
		sendNormalCh: make(chan pendingHTTP3Send, HTTP3TransportSendNormalBuffer),
		sendLowCh:    make(chan pendingHTTP3Send, HTTP3TransportSendLowBuffer),
		backoff:      time.Second,      // Initial backoff at 1 second
		maxBackoff:   30 * time.Second, // Maximum backoff interval should be reasonable for production
	}

	go h.receiveLoop()

	// Start send workers for each priority level (HTTP/3 supports multiplexing)
	h.sendWorkerWg.Add(3)
	go func() { defer h.sendWorkerWg.Done(); h.sendWorker(h.sendHighCh) }()
	go func() { defer h.sendWorkerWg.Done(); h.sendWorker(h.sendNormalCh) }()
	go func() { defer h.sendWorkerWg.Done(); h.sendWorker(h.sendLowCh) }()

	return h, nil
}

func (h *HTTP3Transport) receiveLoop() {
	for {
		select {
		case f := <-h.recvCh:
			if f == nil {
				return
			}
			h.recvMu.RLock()
			cb := h.cb
			h.recvMu.RUnlock()
			if cb != nil {
				cb(f)
			} else {
				frame.PutFrame(f)
			}
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *HTTP3Transport) Connect() error {
	sseCtx, sseCancel := context.WithCancel(h.ctx)
	h.sseCancel = sseCancel

	// Start SSE loop in background
	go h.sseLoop(sseCtx)

	logger.S().Infow("HTTP/3 transport connected, SSE stream starting", "addr", h.addr)
	return nil
}

func (h *HTTP3Transport) sseLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return // Exit cleanly on context cancellation
		default:
		}

		streamURL := fmt.Sprintf("https://%s/stream", h.addr)
		req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
		if err != nil {
			logger.S().Warnw("HTTP/3 sse request failed", "err", err)
			time.Sleep(h.backoff)
			h.backoff = h.minDuration(h.backoff*2, h.maxBackoff)
			continue
		}
		req.Header.Set("Accept", "text/event-stream")
		// Use Authorization header instead of query param for security (token not in access logs)
		req.Header.Set("Authorization", "Bearer "+h.token)

		resp, err := h.client.Do(req)
		if err != nil {
			logger.S().Warnw("HTTP/3 sse connect failed", "err", err)
			time.Sleep(h.backoff)
			h.backoff = h.minDuration(h.backoff*2, h.maxBackoff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			logger.S().Warnw("HTTP/3 sse bad status", "status", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(h.backoff)
			h.backoff = h.minDuration(h.backoff*2, h.maxBackoff)
			continue
		}

		logger.S().Infow("HTTP/3 SSE stream connected", "addr", h.addr)
		h.backoff = time.Second // Reset backoff on successful connection

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 16*1024*1024), 16*1024*1024) // Scanner buffer = 16MB for large file transfers
		var eventType string
		var eventData string

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				resp.Body.Close()
				return
			default:
			}

			line := scanner.Text()

			if line == "" {
				// End of event, process if we have a frame event
				if eventType == "frame" && eventData != "" {
					decoded, err := base64.StdEncoding.DecodeString(
						strings.ReplaceAll(eventData, "\n", ""),
					)
					if err != nil {
						logger.S().Warnw("HTTP/3 sse decode failed", "err", err)
						eventType = ""
						eventData = ""
						continue
					}

					f := frame.GetFrame()
					if derr := frame.DecodeInto(f, decoded); derr != nil {
						frame.PutFrame(f)
						logger.S().Warnw("HTTP/3 sse frame decode failed", "err", derr)
						eventType = ""
						eventData = ""
						continue
					}

					select {
					case h.recvCh <- f:
					default:
						frame.PutFrame(f)
						logger.S().Warn("HTTP/3 recv channel full")
					}
				}

				eventType = ""
				eventData = ""
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				newData := strings.TrimPrefix(line, "data: ")
				if eventData == "" {
					eventData = newData
				} else {
					eventData += "\n" + newData
				}
			}
		}

		resp.Body.Close()

		if err := scanner.Err(); err != nil {
			logger.S().Warnw("HTTP/3 sse scanner error", "err", err)
		}

		logger.S().Infow("HTTP/3 SSE connection lost, retrying", "addr", h.addr)
		time.Sleep(h.backoff)
		h.backoff = h.minDuration(h.backoff*2, h.maxBackoff)
	}
}

// SendFrame encodes frame and puts it in priority send queue.
// sendLoop never blocks - just puts into channel and returns.
func (h *HTTP3Transport) SendFrame(f *frame.Frame) error {
	h.closeMu.Lock()
	if h.closed {
		h.closeMu.Unlock()
		return fmt.Errorf("HTTP/3 transport closed")
	}
	h.closeMu.Unlock()

	data, err := frame.EncodePooled(f)
	if err != nil {
		return fmt.Errorf("frame encode failed: %w", err)
	}

	// Select channel by frame priority flag
	var ch chan pendingHTTP3Send
	priority := f.GetPriority() & 0x1F // extract priority from lower 5 bits

	switch priority {
	case frame.FlagPriorityHigh, frame.FlagPrioritySystem:
		ch = h.sendHighCh
	case frame.FlagPriorityLow:
		ch = h.sendLowCh
	default:
		ch = h.sendNormalCh
	}

	// Non-blocking send to channel
	select {
	case ch <- pendingHTTP3Send{frameType: f.Type, data: data}:
		return nil
	case <-h.ctx.Done():
		// transport is shutting down: release buffer and return error
		frame.ReleaseEncoded(data)
		return fmt.Errorf("HTTP/3 transport closed")
	default:
		// queue full: release pooled buffer and return error
		frame.ReleaseEncoded(data)
		priorityName := "normal"
		switch priority {
		case frame.FlagPriorityHigh, frame.FlagPrioritySystem:
			priorityName = "high"
		case frame.FlagPriorityLow:
			priorityName = "low"
		}
		return fmt.Errorf("HTTP/3 %s send queue full", priorityName)
	}
}

// sendWorker makes actual HTTP POST requests for frames of specified priority.
// Each goroutine works independently, allowing multiplexing within one HTTP/3 connection.
func (h *HTTP3Transport) sendWorker(ch chan pendingHTTP3Send) {
	for {
		select {
		case ps, ok := <-ch: // comma-ok for correct channel closing
			if !ok {
				return
			}
			h.doHTTPPost(ps.frameType, ps.data)
		case <-h.ctx.Done():
			return
		}
	}
}

// doHTTPPost performs blocking HTTP POST request for frame.
// Called from sendWorker goroutine, so it doesn't block sendLoop.
func (h *HTTP3Transport) doHTTPPost(frameType frame.FrameType, data []byte) {
	req, err := http.NewRequestWithContext(h.ctx, "POST", "https://"+h.addr+"/frames", bytes.NewReader(data))
	if err != nil {
		logger.S().Errorw("HTTP/3 request creation failed", "err", err)
		frame.ReleaseEncoded(data)
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := h.client.Do(req)
	if err != nil {
		logger.S().Errorw("HTTP/3 send failed", "err", err)
		frame.ReleaseEncoded(data)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// For HELLO response ONLY, read body as frame to get route information
	if frameType == frame.FrameHello && resp.StatusCode == http.StatusOK {
		respData, err := io.ReadAll(resp.Body)
		if err != nil {
			logger.S().Errorw("failed to read HELLO response", "err", err)
			frame.ReleaseEncoded(data)
			return
		}

		if len(respData) > 0 {
			h.recvMu.RLock()
			cb := h.cb
			h.recvMu.RUnlock()

			if cb != nil {
				respFrame := frame.GetFrame()
				if derr := frame.DecodeInto(respFrame, respData); derr == nil {
					select {
					case h.recvCh <- respFrame:
					default:
						frame.PutFrame(respFrame)
						logger.S().Warn("HTTP/3 recv channel full")
					}
				} else {
					frame.PutFrame(respFrame)
					logger.S().Errorw("decode HELLO response failed", "err", derr)
				}
			}
		}
		frame.ReleaseEncoded(data)
		return // HELLO response processed completely
	}

	// For AUTH: server responds with plain text "authenticated" (no FrameRoute over HTTP).
	// Inject a synthetic FrameRoute so OnConnect fires, matching ws/quic/grpc behaviour.
	if frameType == frame.FrameAuth && resp.StatusCode == http.StatusOK {
		routeFrame := frame.GetFrame()
		routeFrame.Type = frame.FrameRoute
		routeFrame.Version = 1
		routeFrame.Payload = []byte(`{}`)
		select {
		case h.recvCh <- routeFrame:
		default:
			frame.PutFrame(routeFrame)
			logger.S().Warn("HTTP/3 recv channel full (synthetic route frame)")
		}
		frame.ReleaseEncoded(data)
		return
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.S().Errorw("HTTP/3 send failed", "status", resp.StatusCode, "body", string(body))
	}

	frame.ReleaseEncoded(data)
}

func (h *HTTP3Transport) OnFrame(cb func(*frame.Frame)) {
	h.recvMu.Lock()
	defer h.recvMu.Unlock()

	h.cb = cb
}

func (h *HTTP3Transport) IsConnected() bool {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	return !h.closed
}

func (h *HTTP3Transport) Close() error {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()

	if h.closed {
		return nil
	}

	h.closed = true

	// First cancel context so workers see ctx.Done()
	h.cancel()

	// Close recvCh to stop receiveLoop
	close(h.recvCh)

	// Close send channels AFTER cancel (workers will see ctx.Done() first or channel closed)
	close(h.sendHighCh)
	close(h.sendNormalCh)
	close(h.sendLowCh)

	if h.sseCancel != nil {
		h.sseCancel()
	}

	logger.S().Infow("HTTP/3 transport closing, waiting for workers")

	// Wait for all sendWorkers to complete (in-flight requests will finish)
	h.sendWorkerWg.Wait()

	logger.S().Infow("HTTP/3 transport closed")
	return nil
}

func (h *HTTP3Transport) minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
