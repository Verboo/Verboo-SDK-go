// Package config provides configuration management for VerbooRTC server.
// Configuration sources are loaded in this order (highest to lowest precedence):
// 1. OS environment variables
// 2. Explicit config file (if provided)
// 3. Code defaults set with SetDefault()
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

var cfg *viper.Viper // Global configuration instance

// Init initializes global config with the following precedence:
// 1. OS environment variables (and loaded .env into OS env)
// 2. Explicit config file (if provided)
// 3. Default values set in code
//
// Parameters:
// - envPrefix: optional prefix for environment variables (e.g., "VERBOO")
// - filePath: path to config file (yaml/toml/json); empty for no explicit file
func Init(envPrefix, filePath string) error {
	// Load .env into environment (if exists)
	_ = godotenv.Load()

	v := viper.New()

	// Support nested keys via dot-notation and map env keys like SERVER__ADDR or SERVER_ADDR
	if envPrefix != "" {
		v.SetEnvPrefix(envPrefix)
	}

	v.AutomaticEnv()
	// Map env variable names like SERVER_ADDR or SERVER__ADDR to keys like server.addr
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Default values for common configurations
	v.SetDefault("redis.url", "redis:6379")
	v.SetDefault("nats.url", "nats://localhost:4222")
	v.SetDefault("server.server_id", "server-1")
	v.SetDefault("server.addr", ":8443")
	v.SetDefault("server.quic_listen", ":8445")
	v.SetDefault("server.http3_listen", ":8444")
	v.SetDefault("server.alpn", []string{"verboo-rtc"})
	v.SetDefault("server.max_frame_payload_size", 2*1024*1024) // default 2 MiB

	// Read config file if provided
	if filePath != "" {
		v.SetConfigFile(filePath)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read config file %s: %w", filePath, err)
		}
	}

	cfg = v
	return nil
}

// V returns underlying viper instance (may be nil if Init not called).
func V() *viper.Viper { return cfg }

// Generic get functions for common types

// GetString retrieves string value from configuration
func GetString(key string) string {
	if cfg == nil {
		return ""
	}
	return cfg.GetString(key)
}

// GetBool retrieves boolean value from configuration
func GetBool(key string) bool {
	if cfg == nil {
		return false
	}
	return cfg.GetBool(key)
}

// GetInt retrieves integer value from configuration
func GetInt(key string) int {
	if cfg == nil {
		return 0
	}
	return cfg.GetInt(key)
}

// GetInt64 retrieves int64 value from configuration
func GetInt64(key string) int64 {
	if cfg == nil {
		return 0
	}
	return cfg.GetInt64(key)
}

// GetDuration retrieves duration value from configuration
func GetDuration(key string, fallback time.Duration) time.Duration {
	if cfg == nil {
		return fallback
	}
	// Viper supports both string duration and numeric seconds for durations
	if s := cfg.GetString(key); s != "" {
		d, err := time.ParseDuration(s)
		if err == nil {
			return d
		}
	}
	if n := cfg.GetInt64(key); n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

// GetStringSlice retrieves slice of strings from configuration
func GetStringSlice(key string) []string {
	if cfg == nil {
		return nil
	}
	val := cfg.Get(key)
	switch t := val.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, v := range t {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		out := []string{}
		for _, p := range strings.Split(t, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}

// GetStringMap retrieves map of strings from configuration
func GetStringMap(key string) map[string]interface{} {
	if cfg == nil {
		return nil
	}
	return cfg.GetStringMap(key)
}

// MaxFramePayloadSize returns configured maximum allowed size of a frame payload.
func MaxFramePayloadSize() int64 {
	if cfg == nil {
		return 2 * 1024 * 1024 // default 2 MiB
	}
	size := cfg.GetInt64("server.max_frame_payload_size")
	if size <= 0 {
		return 2 * 1024 * 1024
	}
	return size
}

// SetJWTSecretFromProvider sets JWT secret programmatically (useful for tests or secret-manager injection)
func SetJWTSecretFromProvider(secret string) {
	if cfg == nil {
		cfg = viper.New()
		cfg.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	}
	cfg.Set("jwt.secret", secret)
}

// JWSecret returns configured JWT secret (dot key "jwt.secret" or env variable JWT_SECRET)
func JWSecret() string {
	if cfg == nil {
		return ""
	}
	// Check environment variable style first (common for production)
	if s := cfg.GetString("JWT_SECRET"); s != "" {
		return s
	}
	return cfg.GetString("jwt.secret")
}
