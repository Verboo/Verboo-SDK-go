package pb

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// makeTestJWT creates an HMAC-SHA256 JWT with claims "user_id", "exp", and "iat".
// Use only in tests. `secret` may be any string used to sign the token.
func makeTestJWT(secret, userID string) string {
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(5 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	ss, _ := t.SignedString([]byte(secret))
	return ss
}
