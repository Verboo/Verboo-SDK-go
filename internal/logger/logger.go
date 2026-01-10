// Package logger provides a simple wrapper around Zap's SugaredLogger for VerbooRTC.
//
// Usage:
// - Call Init(sug) with a *zap.SugaredLogger to initialize the global logger
// - Use S() to get the global logger instance anywhere in code
package logger

import (
	"go.uber.org/zap"
)

var sugar *zap.SugaredLogger // Global logger instance

// Init initializes global sugared logger. Call once at startup.
// Caller should defer logger.Sync() if needed to flush logs on shutdown.
func Init(l *zap.SugaredLogger) {
	sugar = l
}

// S returns the global *zap.SugaredLogger. If not initialized returns a nop logger.
func S() *zap.SugaredLogger {
	if sugar == nil {
		nop := zap.NewNop().Sugar()
		return nop
	}
	return sugar
}
