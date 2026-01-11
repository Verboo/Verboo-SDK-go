// Package auth provides authentication helpers for the server. Functions in
// this package parse and validate authentication payloads supplied by clients
// and return an authenticated user identifier. Do not hardcode secrets in
// production; read them from configuration or a secret store.
package auth

import (
	"encoding/json"
	"fmt"

	"github.com/Verboo/Verboo-SDK-go/pkg/config"
)

// AuthenticateUser validates an authentication payload (JSON with fields
// "token" and optional "user_id"), verifies the JWT token and returns the
// authenticated user ID.
//
// JWT signing secret is read from configuration via internal/config (JWT_SECRET).
// In production ensure JWT_SECRET is provided via env, config file, or secret manager.
func AuthenticateUser(authPayload []byte) (string, error) {
	var authData struct {
		Token  string `json:"token"`   // JWT token to validate
		UserID string `json:"user_id"` // Optional user ID for additional validation
	}

	if err := json.Unmarshal(authPayload, &authData); err != nil {
		return "", fmt.Errorf("invalid auth payload: %w", err)
	}

	// Read JWT secret from config (must be provided in production)
	secret := config.JWSecret()
	if secret == "" {
		return "", fmt.Errorf("JWT secret not configured")
	}

	// Validate JWT token
	claims, err := ValidateToken(authData.Token, secret)
	if err != nil {
		return "", fmt.Errorf("token validation failed: %w", err)
	}

	// Extract user ID from claims
	userID, ok := claims["user_id"].(string)
	if !ok || userID == "" {
		return "", fmt.Errorf("invalid user ID in token")
	}

	return userID, nil
}
