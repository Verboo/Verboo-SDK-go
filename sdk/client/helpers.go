package client

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GenerateToken generates a JWT token using the provided secret and user ID.
func GenerateToken(userID string, secret string) (string, error) {
	if secret == "" {
		secret = "JWT-Secret-key" // Default for development
	}

	now := time.Now().Unix()
	claims := jwt.MapClaims{
		"user_id": userID,
		"iat":     float64(now),
		"exp":     float64(now + int64(24*time.Hour.Seconds())),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", err
	}

	return signedToken, nil
}

// min is a helper function for limiting string length.
func minimal(a, b int) int {
	if a < b {
		return a
	}
	return b
}
