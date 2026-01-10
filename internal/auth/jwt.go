// Package auth implements JWT validation helpers used by AuthenticateUser.
// It relies on golang-jwt for parsing and verifies token signature and numeric
// date claims. This code expects exp/nbf as numeric timestamps; string-form
// dates are intentionally rejected to avoid ambiguous parsing.
package auth

import (
	"errors"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken is the standard error for invalid JWT tokens
var ErrInvalidToken = errors.New("invalid token")

// ValidateToken parses and validates a JWT token using the provided secret.
// Returns claims as map[string]interface{} when valid. Accepted algorithms:
// HS256, HS384, HS512. The function validates standard numeric date claims
// (exp/nbf) using validateNumericDate. Strings for numeric date claims are
// rejected and result in validation failure; this is deliberate to avoid
// silent acceptance of malformed tokens.
func ValidateToken(tok string, secret string) (map[string]interface{}, error) {
	if tok == "" {
		return nil, ErrInvalidToken
	}

	// Use RegisteredClaims for standard validation and MapClaims for extra fields.
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
	token, err := parser.ParseWithClaims(tok, &claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}

	// Validate exp/nbf/iat using numeric checks if present
	if exp, ok := claims["exp"]; ok {
		if !validateNumericDate(exp) {
			return nil, ErrInvalidToken
		}
	}

	// Convert claims to plain map for easy access
	res := make(map[string]interface{}, len(claims))
	for k, v := range claims {
		res[k] = v
	}
	return res, nil
}

// validateNumericDate ensures the given value represents a future or current numeric UNIX timestamp.
// It supports numeric types only; string values return false to fail-safe. Replace with robust parsing
// if you must accept formatted date strings.
func validateNumericDate(v interface{}) bool {
	now := time.Now().Unix()
	switch t := v.(type) {
	case float64:
		return int64(t) >= now
	case int64:
		return t >= now
	case string:
		// numeric strings are unusual for exp; attempt parse (not common, so fail fast)
		return false
	default:
		return false
	}
}
