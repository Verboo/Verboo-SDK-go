package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/internal/logger"
	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	pb "github.com/Verboo/Verboo-SDK-go/protos/gen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
)

type jwtCredentials struct {
	token string
}

func (c *jwtCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

func (c *jwtCredentials) RequireTransportSecurity() bool {
	return true // требует TLS для безопасности
}

// GRPCTransport implements Transport interface for Verboo-RTC using gRPC bidirectional stream
type GRPCTransport struct {
	addr     string
	jwtToken string // JWT token for authentication

	mu     sync.RWMutex // Mutex for connection state
	conn   *grpc.ClientConn
	stream pb.Signaling_SignalClient

	// Backoff configuration for reconnect attempts
	backoff    time.Duration // Current backoff duration
	maxBackoff time.Duration // Maximum backoff interval

	insecure  bool        // Используем отдельное поле вместо opts
	tlsConfig *tls.Config // Новое поле для хранения TLS-конфигурации

	recvMu sync.RWMutex       // Добавлено для синхронизации OnFrame
	cb     func(*frame.Frame) // Добавлено для callback в OnFrame
	cancel context.CancelFunc
	ctx    context.Context
}

func NewGRPCClient(opts Options) (Transport, error) {
	ctx, cancel := context.WithCancel(context.Background())

	return &GRPCTransport{
		addr:       opts.Addr,
		jwtToken:   opts.Token,
		ctx:        ctx,
		cancel:     cancel,
		backoff:    time.Second,      // Initial backoff at 1 second
		maxBackoff: 30 * time.Second, // Maximum backoff interval should be reasonable for production
		insecure:   opts.Insecure,
		tlsConfig:  opts.TlsCfg, // Передаем TLS-конфигурацию
	}, nil
}

func (g *GRPCTransport) Connect() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.conn != nil {
		return nil
	}

	var tlsCfg *tls.Config = &tls.Config{
		InsecureSkipVerify: g.insecure,
	}
	if g.tlsConfig != nil {
		tlsCfg = g.tlsConfig
	}

	var dialOpts []grpc.DialOption

	if g.jwtToken != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(&jwtCredentials{token: g.jwtToken}))
	}

	if tlsCfg != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(g.addr, dialOpts...)
	if err != nil {
		return fmt.Errorf("gRPC dial failed: %w", err)
	}
	g.conn = conn

	client := pb.NewSignalingClient(conn)
	g.stream, err = client.Signal(g.ctx)
	if err != nil {
		conn.Close()
		return fmt.Errorf("gRPC Signal stream open failed: %w", err)
	}

	go g.recvLoop()

	logger.S().Infow("gRPC transport connected", "addr", g.addr)
	return nil
}

func (g *GRPCTransport) recvLoop() {
	for {
		msg, err := g.stream.Recv()
		if err != nil {
			if grpc.Code(err) == codes.Canceled || strings.Contains(err.Error(), "context canceled") {
				logger.S().Infow("gRPC stream closed", "err", err)
			} else {
				logger.S().Warnw("gRPC stream error", "err", err)
			}
			if err := g.reconnect(); err != nil {
				logger.S().Warnw("gRPC reconnect failed", "err", err)
			}
			return
		}

		f := frame.GetFrame()
		if derr := frame.DecodeInto(f, msg.Payload); derr != nil {
			frame.PutFrame(f)
			logger.S().Warnw("gRPC frame decode failed", "err", derr)
			continue
		}

		g.recvMu.RLock()
		cb := g.cb
		g.recvMu.RUnlock()

		if cb != nil {
			// Transfer ownership to callback; the callback must call frame.PutFrame when done
			cb(f)
		} else {
			// No callback — return to pool
			frame.PutFrame(f)
		}
	}
}

func (g *GRPCTransport) OnFrame(cb func(*frame.Frame)) {
	g.recvMu.Lock()
	defer g.recvMu.Unlock()

	g.cb = cb
}

func (g *GRPCTransport) SendFrame(f *frame.Frame) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stream == nil || g.conn == nil {
		return fmt.Errorf("gRPC stream not initialized")
	}

	data, err := frame.EncodePooled(f)
	if err != nil {
		return fmt.Errorf("frame encode failed: %w", err)
	}
	defer frame.ReleaseEncoded(data)

	if err := g.stream.Send(&pb.FrameMessage{Payload: data}); err != nil {
		frame.ReleaseEncoded(data)
		return fmt.Errorf("gRPC send failed: %w", err)
	}

	logger.S().Debugw("gRPC frame sent", "type", f.Type, "size", len(data))
	return nil
}

func (g *GRPCTransport) IsConnected() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conn != nil
}

func (g *GRPCTransport) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.conn == nil {
		return nil
	}

	if g.stream != nil {
		g.stream.CloseSend()
	}
	if g.conn != nil {
		g.conn.Close()
	}

	g.cancel()

	logger.S().Infow("gRPC transport closed")
	return nil
}

func (g *GRPCTransport) minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (g *GRPCTransport) reconnect() error {
	if err := g.Connect(); err != nil {
		logger.S().Warnw("gRPC reconnect failed", "err", err)
		time.Sleep(g.backoff)
		g.backoff = g.minDuration(g.backoff*2, g.maxBackoff)
		return err
	}
	g.backoff = time.Second // Reset backoff on successful reconnect
	logger.S().Infow("gRPC reconnected successfully")
	return nil
}
