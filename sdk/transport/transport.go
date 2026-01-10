package transport

import (
	"crypto/tls"
	"time"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
)

type Options struct {
	Addr     string        // Server address
	Token    string        // JWT token
	Insecure bool          // Secure/Insecure mode
	Timeout  time.Duration // Connection timeout
	Mode     string        // Added mode field to match usage
	TlsCfg   *tls.Config   // TLS configuration
}

// Transport is the interface for all transports.
type Transport interface {
	Connect() error
	SendFrame(f *frame.Frame) error
	OnFrame(cb func(*frame.Frame))
	Close() error
	IsConnected() bool
}

func CreateTransport(options Options) (Transport, error) {
	switch options.Mode {
	case "ws":
		return NewWsTransport(Options{
			Addr:     options.Addr,
			Token:    options.Token,
			Insecure: options.Insecure,
			Timeout:  options.Timeout,
		})
	case "h2", "http2":
		return NewHTTP2Transport(Options{
			Addr:     options.Addr,
			Token:    options.Token,
			Insecure: options.Insecure,
			Timeout:  options.Timeout,
		})
	case "h3", "http3":
		return NewHTTP3Transport(Options{
			Addr:     options.Addr,
			Token:    options.Token,
			Insecure: options.Insecure,
			Timeout:  options.Timeout,
		})
	case "quic":
		return NewQuicTransport(Options{
			Addr:     options.Addr,
			Token:    options.Token,
			Insecure: options.Insecure,
			Timeout:  options.Timeout,
		})
	case "grpc":
		return NewGRPCClient(Options{
			Addr:     options.Addr,
			Token:    options.Token,
			Insecure: options.Insecure,
		})

	default:
		return nil, ErrUnsupportedTransport
	}
}

var (
	ErrUnsupportedTransport = &sdkError{
		code:   "unsupported_transport",
		msg:    "transport mode not supported (ws/h2/h3/quic)",
		isUser: true,
	}
)

type sdkError struct {
	code   string
	msg    string
	isUser bool
}

func (e *sdkError) Error() string { return e.msg }
