package client

import (
	"crypto/tls"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"io"
	"os"
	"time"
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

	// File transfer options
	FileChunkSize        int                     // Chunk size for file transfers (default: 64 KiB)
	DownloadDir          string                  // Directory to save received files (default: "./downloads")
	UploadDir            string                  // Optional directory for uploaded files
	AutoDownloadFiles    bool                    // Auto-download files when notification received (default: true)
	FileProgressCallback func(sent, total int64) // Callback for send progress updates

	// Logging options
	LogOutput zapcore.WriteSyncer // Custom log output (nil = os.Stderr by default)
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

// WithLogOutput sets the log output writer. When nil, uses os.Stderr.
// Use this for TUI apps to redirect logs away from terminal buffer.
func WithLogOutput(w io.Writer) Option {
	return func(o *Options) {
		o.LogOutput = zapcore.AddSync(w)
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

// WithFileChunkSize sets chunk size for file transfers (default: 64 KiB).
func WithFileChunkSize(size int) Option {
	return func(o *Options) {
		o.FileChunkSize = size
	}
}

// WithDownloadDir sets directory to save received files.
func WithDownloadDir(dir string) Option {
	return func(o *Options) {
		o.DownloadDir = dir
	}
}

// WithUploadDir sets directory for uploaded files (optional).
func WithUploadDir(dir string) Option {
	return func(o *Options) {
		o.UploadDir = dir
	}
}

// WithFileProgressCallback sets callback for file transfer progress updates.
func WithFileProgressCallback(cb func(sent, total int64)) Option {
	return func(o *Options) {
		o.FileProgressCallback = cb
	}
}

// newOptions creates a default Options structure with defaults.
func newOptions(opts []Option) *Options {
	opt := &Options{
		Token:             "",   // Token must be set by caller
		Mode:              "ws", // Default transport is WebSocket
		Reconnect:         true, // Default to enable reconnect
		MinBackoff:        time.Second,
		MaxBackoff:        30 * time.Second,
		Insecure:          false,
		Debug:             false, // Default to off
		Timeout:           8 * time.Second,
		UserID:            "default-user", // Default user ID
		AutoDownloadFiles: true,           // Auto-download files by default
	}

	for _, o := range opts {
		o(opt)
	}

	if opt.UserID == "" {
		opt.UserID = "default-user"
	}

	if opt.Logger == nil {
		// Determine log output: custom LogOutput or default os.Stderr
		output := zapcore.AddSync(os.Stderr)
		// Default to stderr
		if opt.LogOutput != nil {
			output = opt.LogOutput
		}

		var core zapcore.Core
		if opt.Debug {
			enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
			core = zapcore.NewCore(enc, output, zapcore.DebugLevel)
		} else {
			enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
			core = zapcore.NewCore(enc, output, zapcore.InfoLevel)
		}
		opt.Logger = zap.New(core).Sugar()
	}

	return opt
}
