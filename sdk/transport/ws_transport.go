package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/logger"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/gorilla/websocket"
)

// Priority buffers for WebSocket transport.
// writeHighCh: heartbeats, control frames (critically important not to block)
// writeNormalCh: normal messages
// writeLowCh: file chunks (large buffer for backpressure)
const (
	WsTransportWriteHighBuffer   = 64 * 1024        // for high priority frames
	WsTransportWriteNormalBuffer = 256 * 1024       // for normal priority frames
	WsTransportWriteLowBuffer    = 1024 * 1024      // for low priority frames (file chunks)
	wsInitialBackoff             = time.Second      // starting backoff interval
	wsMaxBackoff                 = 30 * time.Second // cap on backoff interval

	// WebSocket connection buffer sizes - must match server settings
	WsTransportReadBufferSize  = 256 * 1024 // set read buffer size to 256KB for large chunks
	WsTransportWriteBufferSize = 256 * 1024 // set write buffer size to 256KB for large chunks
)

// WsTransport: robust WebSocket binary transport that implements Transport interface.
type WsTransport struct {
	urlStr string
	opts   Options

	mu     sync.RWMutex
	conn   *websocket.Conn
	recvCb func(*frame.Frame)
	ready  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	// Priority channels for sending frames.
	writeHighCh   chan []byte // HIGH priority: heartbeats, control frames
	writeNormalCh chan []byte // NORMAL priority: messages
	writeLowCh    chan []byte // LOW priority: file chunks

	connected bool
	wg        sync.WaitGroup // tracks readPump instances, writePump, reconnectLoop

	// Reconnect state.
	// disconnectCh is buffered(1) so readPump never blocks on signal.
	disconnectCh chan struct{}
	backoff      time.Duration
	maxBackoff   time.Duration
	onReconnect  func() // called in a goroutine after each successful reconnect
}

func NewWsTransport(opts Options) (*WsTransport, error) {
	if opts.Addr == "" {
		return nil, fmt.Errorf("address required for ws transport")
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &WsTransport{
		urlStr:        opts.Addr,
		opts:          opts,
		ready:         make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
		writeHighCh:   make(chan []byte, WsTransportWriteHighBuffer),
		writeNormalCh: make(chan []byte, WsTransportWriteNormalBuffer),
		writeLowCh:    make(chan []byte, WsTransportWriteLowBuffer),
		connected:     false,
		disconnectCh:  make(chan struct{}, 1),
		backoff:       wsInitialBackoff,
		maxBackoff:    wsMaxBackoff,
	}, nil
}

// OnReconnect registers a callback invoked (in a new goroutine) after each
// successful reconnect. Use it to redo application-level handshake (HELLO/AUTH).
func (w *WsTransport) OnReconnect(cb func()) {
	w.mu.Lock()
	w.onReconnect = cb
	w.mu.Unlock()
}

// dial opens a new WebSocket connection and atomically updates w.conn / w.connected.
// It is safe to call from any goroutine; it does NOT start any pumps.
func (w *WsTransport) dial() error {
	u := url.URL{Scheme: "wss", Host: w.urlStr, Path: "/ws"}

	dialer := websocket.Dialer{
		HandshakeTimeout: w.opts.Timeout,
		// Set buffer sizes to match server settings for large chunk transfers
		ReadBufferSize:  WsTransportReadBufferSize,  // 256KB read buffer
		WriteBufferSize: WsTransportWriteBufferSize, // 256KB write buffer
		TLSClientConfig: &tls.Config{InsecureSkipVerify: w.opts.Insecure},
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}

	w.mu.Lock()
	w.conn = conn
	w.connected = true
	w.mu.Unlock()

	return nil
}

// Connect dials the server and starts the write pump, initial read pump,
// and the reconnect loop.  It must be called exactly once.
func (w *WsTransport) Connect() error {
	if err := w.dial(); err != nil {
		return err
	}

	// writePump runs for the entire transport lifetime (ctx cancellation stops it).
	w.wg.Add(1)
	go func() { defer w.wg.Done(); w.writePump() }()

	// readPump handles the current connection; restarted by reconnectLoop on drop.
	w.wg.Add(1)
	go func() { defer w.wg.Done(); w.readPump() }()

	// reconnectLoop monitors disconnects and re-dials with exponential backoff.
	w.wg.Add(1)
	go func() { defer w.wg.Done(); w.reconnectLoop() }()

	close(w.ready)
	logger.S().Infow("ws connected", "url", "wss://"+w.urlStr+"/ws")
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

		// Signal reconnect loop only on unexpected disconnect.
		// If the context is already cancelled (graceful Close), skip the signal.
		select {
		case <-w.ctx.Done():
			// Transport is shutting down - reconnect is not desired.
		default:
			// Non-blocking send: buffer is 1, so at most one pending signal at a time.
			select {
			case w.disconnectCh <- struct{}{}:
			default:
			}
		}
	}()

	w.conn.SetReadLimit(16 << 20) // 16 MiB limit per message for large file transfers
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
			cb(f) // ownership transferred to cb; cb must call frame.PutFrame when done
		} else {
			frame.PutFrame(f) // no consumer - return to pool
		}
	}
}

// reconnectLoop waits for unexpected connection drops and re-establishes the
// WebSocket connection with exponential backoff.  It runs for the full transport
// lifetime and exits when the context is cancelled (i.e. on Close).
func (w *WsTransport) reconnectLoop() {
	for {
		// Block until a disconnect signal arrives or the transport is closed.
		select {
		case <-w.ctx.Done():
			return
		case <-w.disconnectCh:
		}

		logger.S().Warnw("ws connection lost, starting reconnect")

		// Inner backoff loop: keep retrying until successful or closed.
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(w.backoff):
			}

			logger.S().Infow("ws reconnecting...", "backoff", w.backoff)

			if err := w.dial(); err != nil {
				w.backoff = wsDurationMin(w.backoff*2, w.maxBackoff)
				logger.S().Warnw("ws reconnect failed", "err", err, "next_backoff", w.backoff)
				continue
			}

			// Guard: check ctx again after a potentially slow dial - Close() may
			// have been called while we were connecting.
			select {
			case <-w.ctx.Done():
				// Close the just-opened connection to avoid a leak.
				w.mu.Lock()
				if w.conn != nil {
					_ = w.conn.Close()
					w.conn = nil
				}
				w.connected = false
				w.mu.Unlock()
				return
			default:
			}

			w.backoff = wsInitialBackoff // reset on success
			logger.S().Infow("ws reconnected successfully")

			// Restart read pump for the new connection.
			// writePump keeps running and picks up the new conn through w.conn (RLocked).
			w.wg.Add(1)
			go func() { defer w.wg.Done(); w.readPump() }()

			// Notify caller so it can redo the application-level handshake.
			w.mu.RLock()
			cb := w.onReconnect
			w.mu.RUnlock()
			if cb != nil {
				go cb()
			}

			break // back to outer select: wait for next disconnect
		}
	}
}

func (w *WsTransport) writePump() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		// 1. First check HIGH priority with non-blocking select
		select {
		case b := <-w.writeHighCh:
			w.doWrite(b)
			continue // always return to high priority
		default:
		}

		// 2. HIGH + NORMAL + LOW - blocking wait with high priority first
		select {
		case b := <-w.writeHighCh:
			w.doWrite(b)
		case b := <-w.writeNormalCh:
			w.doWrite(b)
		case b := <-w.writeLowCh:
			// Soft backpressure for low priority
			if len(w.writeLowCh) > 800 { // >75% of buffer
				time.Sleep(time.Millisecond)
			}
			w.doWrite(b)
		case <-ticker.C:
			// Periodic ping to keep connection alive
			w.mu.RLock()
			c := w.conn
			w.mu.RUnlock()
			if c != nil {
				_ = c.WriteControl(websocket.PingMessage,
					[]byte("ping"), time.Now().Add(5*time.Second))
			}
		case <-w.ctx.Done():
			return
		}
	}
}

// doWrite performs frame write to WebSocket connection.
// Sets WriteDeadline before writing and releases buffer after.
func (w *WsTransport) doWrite(b []byte) {
	w.mu.RLock()
	c := w.conn
	w.mu.RUnlock()

	if c == nil {
		// During a reconnect window the connection is temporarily nil.
		// Drop the frame and release its buffer - consistent with other transports.
		logger.S().Debug("ws write: conn nil, dropping frame during reconnect")
		frame.ReleaseEncoded(b)
		return
	}

	// Set WriteDeadline before each write (protection from timeout during backpressure)
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
		logger.S().Errorw("ws write failed", "err", err)
	}
	frame.ReleaseEncoded(b) // Always release buffer after use
}

// SendFrame encodes frame and puts it in priority send queue.
// sendLoop never blocks - just puts into channel and returns.
func (w *WsTransport) SendFrame(f *frame.Frame) error {
	if err := w.ensureConnected(); err != nil {
		return err
	}

	// Encode frame in pooled buffer
	pb, err := frame.EncodePooled(f)
	if err != nil {
		return fmt.Errorf("frame encode failed: %w", err)
	}

	var ch chan []byte
	priority := f.GetPriority() & 0x1F // extract priority from lower 5 bits

	switch priority {
	case frame.FlagPrioritySystem, frame.FlagPriorityHigh:
		ch = w.writeHighCh
	case frame.FlagPriorityLow:
		ch = w.writeLowCh
	default:
		ch = w.writeNormalCh
	}

	// Non-blocking send to channel
	select {
	case ch <- pb:
		return nil
	case <-w.ctx.Done():
		// transport is shutting down: release buffer and return error
		frame.ReleaseEncoded(pb)
		return fmt.Errorf("ws transport closed")
	default:
		// queue full: release pooled buffer and return error
		frame.ReleaseEncoded(pb)
		priorityName := "normal"
		switch priority {
		case frame.FlagPrioritySystem, frame.FlagPriorityHigh:
			priorityName = "high"
		case frame.FlagPriorityLow:
			priorityName = "low"
		}
		return fmt.Errorf("ws %s write queue full", priorityName)
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
	// Signal pumps and reconnect loop to stop.
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel() // cancel context: unblocks writePump, reconnectLoop, and backoff sleeps
	}
	// Close underlying websocket connection (best-effort) to unblock readPump.
	if w.conn != nil {
		_ = w.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = w.conn.Close()
		w.conn = nil
	}
	w.connected = false
	// Unlock before waiting to avoid deadlock in pumps that acquire mu.
	w.mu.Unlock()

	// Wait for all tracked goroutines (readPump instances, writePump, reconnectLoop).
	w.wg.Wait()

	// After all goroutines have finished, safely drain and close the write channels.
	w.mu.Lock()
	close(w.writeHighCh)
	close(w.writeNormalCh)
	close(w.writeLowCh)
	w.mu.Unlock()
	return nil
}

// wsDurationMin returns the smaller of two durations.
func wsDurationMin(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
