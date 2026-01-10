package auth_test

import (
	"testing"

	"github.com/Verboo/Verboo-SDK-go/internal/auth"
	"github.com/Verboo/Verboo-SDK-go/internal/config"
	jwt "github.com/golang-jwt/jwt/v5"
	"time"
)

func TestAuthenticateUser_FailsWhenNoSecret(t *testing.T) {
	// ensure config not initialized with secret
	config.Init("", "")
	// create token with some claim
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "u1",
		"exp":     time.Now().Add(time.Minute).Unix(),
	})
	ss, _ := token.SignedString([]byte("dummy"))
	_, err := auth.AuthenticateUser([]byte(`{"token":"` + ss + `"}`))
	if err == nil {
		t.Fatalf("expected error when JWT secret not configured")
	}
}

func TestAuthenticateUser_SucceedsWithConfigSecret(t *testing.T) {
	// set secret via provider helper
	config.Init("", "")
	config.SetJWTSecretFromProvider("test-secret-123")
	// create token signed with same secret
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "u42",
		"exp":     time.Now().Add(time.Minute).Unix(),
	})
	ss, err := token.SignedString([]byte("test-secret-123"))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	uid, err := auth.AuthenticateUser([]byte(`{"token":"` + ss + `"}`))
	if err != nil {
		t.Fatalf("unexpected auth failure: %v", err)
	}
	if uid != "u42" {
		t.Fatalf("unexpected uid: %s", uid)
	}
}
