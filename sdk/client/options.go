package client

import (
	"crypto/tls"
	"github.com/Verboo/Verboo-SDK-go/internal/logger"
	"time"

	"go.uber.org/zap"
)

// Option configures the client.
type Option func(*Options)

// Options contains configuration options for the Verboo-RTC SDK client.
type Options struct {
	Token      string             // JWT token for authentication (required)
	Addr       string             // Server address (host:port)
	Mode       string             // Transport mode: "ws", "quic", "h2", "h3", "grpc"
	Insecure   bool               // Skip TLS verification if true
	TlsCfg     *tls.Config        // TLS configuration for client (optional)
	Reconnect  bool               // Enable automatic reconnect with exponential backoff
	MinBackoff time.Duration      // Minimum backoff interval for reconnections (default 1s)
	MaxBackoff time.Duration      // Maximum backoff interval for reconnections (default 30s)
	Logger     *zap.SugaredLogger // Custom logger for client events
	Debug      bool               // Enable debug logging (default off)
	Timeout    time.Duration      `json:"timeout"` // Timeout
	Secret     string             // Add this field for secret key
	UserID     string             // Add this field for user ID (optional)
}

// WithToken sets the JWT token for authentication.
func WithToken(token string) Option {
	return func(o *Options) {
		o.Token = token
	}
}

// WithServerAddr sets the server address (host:port).
func WithServerAddr(addr string) Option {
	return func(o *Options) {
		o.Addr = addr
	}
}

// WithTransportType specifies which transport to use.
func WithTransportType(mode string) Option {
	return func(o *Options) {
		o.Mode = mode
	}
}

// WithReconnect enables automatic reconnect with exponential backoff.
func WithReconnect(minBackoff, maxBackoff time.Duration) Option {
	return func(o *Options) {
		o.Reconnect = true
		o.MinBackoff = minBackoff
		o.MaxBackoff = maxBackoff
	}
}

// WithLogger sets a custom logger (supports zap.SugaredLogger).
func WithLogger(l *zap.SugaredLogger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithInsecure skips TLS verification.
func WithInsecure() Option {
	return func(o *Options) {
		o.Insecure = true
	}
}

// WithDebug enables debug logging (default off).
func WithDebug(debug bool) Option {
	return func(o *Options) {
		o.Debug = debug
	}
}

// WithTimeout sets the connection timeout duration.
func WithTimeout(d time.Duration) Option {
	return func(o *Options) { // Corrected to use Options
		o.Timeout = d
	}
}

// WithSecretKey sets the JWT secret key used to sign tokens (for development).
func WithSecretKey(secret string) Option {
	return func(o *Options) { // Corrected to use Options
		o.Secret = secret
	}
}

// newOptions creates a default Options structure with defaults.
func newOptions(opts []Option) *Options {
	opt := &Options{
		Token:      "",   // Token must be set by caller
		Mode:       "ws", // Default transport is WebSocket
		Reconnect:  true, // Default to enable reconnect
		MinBackoff: time.Second,
		MaxBackoff: 30 * time.Second,
		Insecure:   false,
		Debug:      false, // Default to off
		Timeout:    8 * time.Second,
		UserID:     "default-user", // Default user ID
	}

	for _, o := range opts {
		o(opt)
	}

	if opt.UserID == "" {
		opt.UserID = "default-user"
	}

	if opt.Logger == nil {
		opt.Logger = logger.S()

		//
		if opt.Debug {
			l, err := zap.NewDevelopment()
			if err == nil {
				opt.Logger = l.Sugar()
			}
		} else {
			l, err := zap.NewProduction()
			if err == nil {
				opt.Logger = l.Sugar()
			}
		}
	}

	return opt
}
