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

	"github.com/quic-go/quic-go/http3"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/Verboo/Verboo-SDK-go/pkg/logger"
)

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

	// Backoff configuration for reconnect attempts
	backoff    time.Duration // Current backoff duration
	maxBackoff time.Duration // Maximum backoff duration

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
		addr:       opts.Addr,
		client:     httpClient,
		token:      opts.Token,
		recvCh:     make(chan *frame.Frame, 128),
		ctx:        ctx,
		cancel:     cancel,
		backoff:    time.Second,      // Initial backoff at 1 second
		maxBackoff: 30 * time.Second, // Maximum backoff interval should be reasonable for production
	}

	go h.receiveLoop()

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

		streamURL := fmt.Sprintf("https://%s/stream?token=%s", h.addr, h.token)
		req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
		if err != nil {
			logger.S().Warnw("HTTP/3 sse request failed", "err", err)
			time.Sleep(h.backoff)
			h.backoff = h.minDuration(h.backoff*2, h.maxBackoff)
			continue
		}
		req.Header.Set("Accept", "text/event-stream")

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
					// Decode base64 encoded frame data
					decoded, err := base64.StdEncoding.DecodeString(eventData)
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
				eventData = strings.TrimPrefix(line, "data: ")
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
	defer frame.ReleaseEncoded(data)

	req, err := http.NewRequestWithContext(h.ctx, "POST", "https://"+h.addr+"/frames", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("http3 request creation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := h.client.Do(req)
	if err != nil {
		frame.ReleaseEncoded(data)
		return fmt.Errorf("http3 send failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// For HELLO response, read body as frame to get route information
	if f.Type == frame.FrameHello && resp.StatusCode == http.StatusOK {
		respData, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read HELLO response: %w", err)
		}

		if len(respData) > 0 {
			respFrame := frame.GetFrame()
			if derr := frame.DecodeInto(respFrame, respData); derr != nil {
				frame.PutFrame(respFrame)
				return fmt.Errorf("decode HELLO response failed: %w", derr)
			}

			select {
			case h.recvCh <- respFrame:
			default:
				frame.PutFrame(respFrame)
				logger.S().Warn("HTTP/3 recv channel full")
			}
		}

		return nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP/3 send failed: status %d body: %s", resp.StatusCode, string(body))
	}

	return nil
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

	// Close recvCh to stop receiveLoop
	close(h.recvCh)

	if h.sseCancel != nil {
		h.sseCancel()
	}
	h.cancel()

	logger.S().Infow("HTTP/3 transport closed")
	return nil
}

func (h *HTTP3Transport) minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
