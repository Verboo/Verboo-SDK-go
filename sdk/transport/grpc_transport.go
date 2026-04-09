package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/logger"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
	pb "github.com/Verboo/Verboo-rtc/protos/gen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
)

type jwtCredentials struct {
	token    string
	insecure bool
}

func (c *jwtCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

func (c *jwtCredentials) RequireTransportSecurity() bool {
	return !c.insecure // Does not require TLS when insecure=true
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

	insecure  bool        // don't require TLS when insecure=true
	tlsConfig *tls.Config // New field to store TLS configuration

	recvMu sync.RWMutex       // Added for OnFrame synchronization
	cb     func(*frame.Frame) // Added for callback in OnFrame
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
		tlsConfig:  opts.TlsCfg, // Pass TLS configuration
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
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(&jwtCredentials{token: g.jwtToken, insecure: g.insecure}))
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

	// Start heartbeat goroutine to keep stream alive (prevents NAT/load balancer timeouts)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-ticker.C:
				// Send heartbeat frame to keep stream alive
				heartbeat := &frame.Frame{
					Type:     frame.FrameHeartbeat,
					Version:  1,
					StreamID: 0,
					Payload:  []byte{},
				}
				if err := g.SendFrame(heartbeat); err != nil {
					logger.S().Debugw("gRPC heartbeat failed", "err", err)
					return
				}
				logger.S().Debug("gRPC heartbeat sent")
			}
		}
	}()

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
			// No callback - return to pool
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

	// REMOVE defer - we call ReleaseEncoded ONLY once after successful send
	if err := g.stream.Send(&pb.FrameMessage{Payload: data}); err != nil {
		frame.ReleaseEncoded(data) // release only on error
		return fmt.Errorf("gRPC send failed: %w", err)
	}

	frame.ReleaseEncoded(data) // release after successful send - ONCE!
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

	if g.stream != nil {
		g.stream.CloseSend()
		g.stream = nil
	}
	if g.conn != nil {
		g.conn.Close()
		g.conn = nil
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
	// Close old connection before attempting to reconnect
	g.mu.Lock()
	if g.stream != nil {
		g.stream.CloseSend()
		g.stream = nil
	}
	if g.conn != nil {
		g.conn.Close()
		g.conn = nil
	}
	g.mu.Unlock()

	// Backoff loop with context check
	for {
		select {
		case <-g.ctx.Done():
			return g.ctx.Err()
		case <-time.After(g.backoff):
		}

		if err := g.Connect(); err == nil {
			g.backoff = time.Second // Reset backoff on success
			logger.S().Infow("gRPC reconnected successfully")
			return nil
		}

		// Exponential backoff on failure
		g.backoff = g.minDuration(g.backoff*2, g.maxBackoff)
		logger.S().Warnw("gRPC reconnect attempt failed", "backoff", g.backoff)
	}
}
