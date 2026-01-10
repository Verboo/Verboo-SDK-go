// ─────── begin of ./sdk/transport/ws_transport.go ───────
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/internal/logger"
	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/gorilla/websocket"
)

// WsTransport: robust WebSocket binary transport that implements Transport interface.
type WsTransport struct {
	urlStr string
	opts   Options

	mu      sync.RWMutex
	conn    *websocket.Conn
	recvCb  func(*frame.Frame)
	ready   chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	writeCh chan []byte

	connected bool
	// per-connection reusable write buffer to support zero-alloc EncodeInto
	writeBuf []byte
	wg       sync.WaitGroup // wait group to coordinate pump shutdown
}

func NewWsTransport(opts Options) (*WsTransport, error) {
	if opts.Addr == "" {
		return nil, fmt.Errorf("address required for ws transport")
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &WsTransport{
		urlStr:    opts.Addr,
		opts:      opts,
		ready:     make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		writeCh:   make(chan []byte, 512),
		connected: false,
	}, nil
}

func (w *WsTransport) Connect() error {
	u := url.URL{Scheme: "wss", Host: w.urlStr, Path: "/ws"} // ИСПРАВЛЕНО: добавляем схему здесь

	dialer := websocket.Dialer{
		HandshakeTimeout: w.opts.Timeout,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: w.opts.Insecure},
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}

	w.mu.Lock()
	w.conn = conn
	w.connected = true
	// preallocate per-connection write buffer to typical MTU-ish size to allow zero-alloc EncodeInto
	if w.writeBuf == nil {
		w.writeBuf = make([]byte, 0, 4096)
	}
	w.mu.Unlock()

	// start pumps and track them with WaitGroup for graceful Close.
	w.wg.Add(2)
	go func() { defer w.wg.Done(); w.readPump() }()
	go func() { defer w.wg.Done(); w.writePump() }()

	close(w.ready)
	logger.S().Infow("ws connected", "url", u.String())
	return nil
}

// ensureConnected waits briefly until the transport is ready.
func (w *WsTransport) ensureConnected() error {
	w.mu.RLock()
	if w.connected && w.conn != nil {
		w.mu.RUnlock()
		return nil
	}
	w.mu.RUnlock()
	select {
	case <-w.ready:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("ws not connected")
	case <-w.ctx.Done():
		return fmt.Errorf("ws context canceled")
	}
}

func (w *WsTransport) readPump() {
	defer func() {
		w.mu.Lock()
		if w.conn != nil {
			_ = w.conn.Close()
			w.conn = nil
		}
		w.connected = false
		w.mu.Unlock()
	}()

	w.conn.SetReadLimit(2 << 20) // 2 MiB limit per message
	_ = w.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	w.conn.SetPongHandler(func(string) error {
		_ = w.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway) {
				logger.S().Debugw("ws unexpected close", "err", err)
			} else {
				logger.S().Debugw("ws read ended", "err", err)
			}
			return
		}

		// get a pooled Frame to avoid per-message allocation
		f := frame.GetFrame()
		if derr := frame.DecodeInto(f, data); derr != nil {
			frame.PutFrame(f) // return frame to pool on error
			logger.S().Errorw("frame decode failed", "err", derr)
			continue
		}

		// synchronous callback: ownership is transferred to callback.
		// Transport will not PutFrame after cb returns. If no callback is
		// registered, return frame to pool to avoid leak.
		w.mu.RLock()
		cb := w.recvCb
		w.mu.RUnlock()
		if cb != nil {
			// ownership transferred to cb; cb must call frame.PutFrame when done
			cb(f)
		} else {
			// no consumer — return to pool
			frame.PutFrame(f)
		}
	}
}

func (w *WsTransport) writePump() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case b, ok := <-w.writeCh:
			if !ok {
				return
			}
			w.mu.RLock()
			c := w.conn
			w.mu.RUnlock()
			if c == nil {
				logger.S().Debug("ws write: conn nil")
				frame.ReleaseEncoded(b)
				continue
			}
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
				logger.S().Errorw("ws write failed", "err", err)
			}
			frame.ReleaseEncoded(b) // Always release buffer after use
		case <-ticker.C:
			w.mu.RLock()
			c := w.conn
			w.mu.RUnlock()
			if c != nil {
				_ = c.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
			}
		}
	}
}

// SendFrame encodes frame and queues it (zero-alloc fast path with direct write fallback).
func (w *WsTransport) SendFrame(f *frame.Frame) error {
	if err := w.ensureConnected(); err != nil {
		return err
	}

	// capture connection and writeBuf under lock to avoid races with Close()
	w.mu.RLock()
	c := w.conn
	writeBuf := w.writeBuf
	w.mu.RUnlock()

	// fast path: try zero-alloc encode into per-connection buffer if available
	if writeBuf != nil {
		if out, err := frame.EncodeInto(writeBuf[:0], f); err == nil {
			// attempt immediate synchronous write using connection owned here
			if c != nil {
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := c.WriteMessage(websocket.BinaryMessage, out); err == nil {
					// successful immediate write, no allocations and no queueing
					return nil
				}
				// immediate write failed -> fallthrough to fallback enqueue
			}
		}
	}

	// fallback path: encode into pooled buffer and enqueue for writePump
	pb, err := frame.EncodePooled(f)
	if err != nil {
		return err
	}

	// Non-blocking enqueue with cancellation awareness to avoid panics
	select {
	case w.writeCh <- pb:
		return nil
	case <-w.ctx.Done():
		// transport is shutting down: release buffer and return error
		frame.ReleaseEncoded(pb)
		return fmt.Errorf("ws transport closed")
	default:
		// queue full: release pooled buffer and return error
		frame.ReleaseEncoded(pb)
		return fmt.Errorf("ws write queue full")
	}
}

func (w *WsTransport) OnFrame(cb func(*frame.Frame)) {
	w.mu.Lock()
	w.recvCb = cb
	w.mu.Unlock()
}

func (w *WsTransport) IsConnected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.connected
}

func (w *WsTransport) Close() error {
	// Signal pumps to stop.
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel() // cancel context to unblock pumps
	}
	// Close underlying websocket connection (best-effort).
	if w.conn != nil {
		_ = w.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = w.conn.Close()
		w.conn = nil
	}
	w.connected = false
	// unlock before waiting to avoid deadlock in pumps that read mu.
	w.mu.Unlock()

	// Wait for read/write pumps to finish.
	w.wg.Wait()

	// After pumps finished, it is safe to close channels and release buffers.
	w.mu.Lock()
	w.writeBuf = nil
	close(w.writeCh)
	w.mu.Unlock()
	return nil
}

//─────── end of ./sdk/transport/ws_transport.go ───────
