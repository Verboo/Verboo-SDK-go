package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	"github.com/Verboo/Verboo-SDK-go/pkg/logger"
)

// QuicTransport implements Transport using quic-go v0.55.x APIs.
type QuicTransport struct {
	addr     string
	tlsCfg   *tls.Config
	jwtToken string

	ctx    context.Context
	cancel context.CancelFunc

	conn      *quic.Conn
	stream    *quic.Stream // bidi control stream
	recvCb    func(*frame.Frame)
	connected bool

	mu      sync.RWMutex
	writeMu sync.Mutex
}

func NewQuicTransport(opts Options) (*QuicTransport, error) {
	if opts.Addr == "" {
		return nil, errors.New("address required for quic transport")
	}
	if opts.Token == "" {
		return nil, errors.New("token is required for quic transport")
	}

	ctx, cancel := context.WithCancel(context.Background())

	tlsCfg := &tls.Config{
		InsecureSkipVerify: opts.Insecure,
		NextProtos:         []string{"verboo-rtc"},
	}

	return &QuicTransport{
		addr:     opts.Addr,
		tlsCfg:   tlsCfg,
		jwtToken: opts.Token,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

func (q *QuicTransport) Connect() error {
	q.mu.Lock()
	if q.connected {
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(q.ctx, 8*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(dialCtx, q.addr, q.tlsCfg, nil)

	if err != nil {
		return err
	}

	str, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		_ = conn.CloseWithError(0, "open stream failed")
		return err
	}

	q.mu.Lock()
	q.conn = conn
	q.stream = str
	q.connected = true
	q.mu.Unlock()

	logger.S().Infow("quic connected", "addr", q.addr)

	go q.readLoop()

	return nil
}

// readLoop now reads length-prefixed frames to handle framing properly
func (q *QuicTransport) readLoop() {
	for {
		q.mu.RLock()
		str := q.stream
		q.mu.RUnlock()
		if str == nil {
			break
		}

		lengthBuf := make([]byte, 4)
		if _, err := io.ReadFull(str, lengthBuf); err != nil {
			if err == io.EOF {
				logger.S().Debugw("quic stream closed by peer")
			} else {
				logger.S().Errorw("quic read length error", "err", err)
			}
			break
		}

		frameLen := binary.BigEndian.Uint32(lengthBuf)

		if frameLen > 10*1024*1024 {
			logger.S().Errorw("frame too large", "len", frameLen)
			break
		}

		frameBuf := make([]byte, frameLen)
		if _, err := io.ReadFull(str, frameBuf); err != nil {
			logger.S().Errorw("quic read frame error", "err", err)
			break
		}

		f := frame.GetFrame()
		logger.S().Debugw("quic read", "frame_len", frameLen, "prefix", fmt.Sprintf("%x", frameBuf[:MinInt(64, len(frameBuf))]))
		if derr := frame.DecodeInto(f, frameBuf); derr != nil {
			frame.PutFrame(f)
			logger.S().Warnw("frame decode failed", "err", derr)
			continue
		}

		q.mu.RLock()
		cb := q.recvCb
		q.mu.RUnlock()
		if cb != nil {
			cb(f)
		} else {
			frame.PutFrame(f)
		}
	}

	q.mu.Lock()
	if q.conn != nil {
		_ = q.conn.CloseWithError(0, "read loop exit")
	}
	q.connected = false
	q.mu.Unlock()
}

// SendFrame now writes length-prefixed frames
func (q *QuicTransport) SendFrame(f *frame.Frame) error {
	q.mu.RLock()
	str := q.stream
	connected := q.connected
	q.mu.RUnlock()

	if !connected || str == nil {
		return errors.New("quic transport not connected")
	}

	b, err := frame.Encode(f)
	if err != nil {
		return err
	}

	lengthBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuf, uint32(len(b)))

	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	logger.S().Debugw("quic write", "frame_len", len(b), "prefix", fmt.Sprintf("%x", b[:MinInt(64, len(b))]))
	if _, err := str.Write(lengthBuf); err != nil {
		logger.S().Warnw("quic write length failed", "err", err)
		return err
	}

	if _, err := str.Write(b); err != nil {
		logger.S().Warnw("quic write frame failed", "err", err)
		return err
	}

	return nil
}

func (q *QuicTransport) OnFrame(cb func(*frame.Frame)) {
	q.mu.Lock()
	q.recvCb = cb
	q.mu.Unlock()
}

func (q *QuicTransport) IsConnected() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.connected
}

func (q *QuicTransport) Close() error {
	q.cancel()

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.stream != nil {
		_ = q.stream.Close()
		q.stream = nil
	}
	if q.conn != nil {
		_ = q.conn.CloseWithError(0, "closing")
		q.conn = nil
	}
	q.connected = false
	return nil
}

func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
