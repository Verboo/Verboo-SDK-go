package auth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ctxKeyUserID is a custom context key type for storing user ID in context
type ctxKeyUserID struct{}

// CtxUserID is the global context key for user ID data
var CtxUserID = ctxKeyUserID{}

// UnaryServerInterceptor returns an interceptor that validates JWT tokens and injects user ID into context.
// It checks if the method is public (e.g., health check) and skips authentication for those.
// For non-public methods, it extracts the token from "Authorization" header and validates it using ValidateToken.
// If validation succeeds, user ID is added to the context with CtxUserID key; otherwise it returns an Unauthenticated error.
//
// Parameters:
// - jwtSecret: Secret string used to verify JWT signature
func UnaryServerInterceptor(jwtSecret string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Check if this method is public (e.g., health check)
		if isPublicUnary(info.FullMethod) {
			return handler(ctx, req)
		}

		userID, err := authenticateFromContext(ctx, jwtSecret)
		if err != nil {
			// Return gRPC Unauthenticated error if auth fails
			return nil, status.Errorf(codes.Unauthenticated, "unauthenticated: %v", err)
		}
		ctx = context.WithValue(ctx, CtxUserID, userID) // Inject user ID into context
		return handler(ctx, req)
	}
}

// StreamServerInterceptor validates JWT tokens for streaming RPCs. Similar to UnaryServerInterceptor but for stream-based communication.
func StreamServerInterceptor(jwtSecret string) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		// Allow public streams like health checks if needed
		if isPublicStream(info.FullMethod) {
			return handler(srv, ss)
		}
		ctx := ss.Context()
		userID, err := authenticateFromContext(ctx, jwtSecret)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "unauthenticated: %v", err)
		}
		wrapped := &wrappedServerStream{ServerStream: ss, ctx: context.WithValue(ctx, CtxUserID, userID)}
		return handler(srv, wrapped)
	}
}

// wrappedServerStream wraps the standard ServerStream to inject user ID context for streaming RPCs
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context // Injected context with user ID
}

// Context returns the wrapped context which contains injected user ID
func (w *wrappedServerStream) Context() context.Context { return w.ctx }

// authenticateFromContext extracts JWT token from metadata and validates it using ValidateToken
func authenticateFromContext(ctx context.Context, jwtSecret string) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrInvalidToken // Missing metadata means no token provided
	}
	auths := md.Get("authorization")
	if len(auths) == 0 {
		return "", ErrInvalidToken
	}
	// Expect "Bearer <token>" format
	parts := strings.SplitN(auths[0], " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", ErrInvalidToken
	}
	token := parts[1]
	claims, err := ValidateToken(token, jwtSecret)
	if err != nil {
		return "", err
	}
	uid, ok := claims["user_id"].(string)
	if !ok || uid == "" {
		return "", ErrInvalidToken // Missing or invalid user ID in claims
	}
	return uid, nil
}

// isPublicUnary checks if a method is public (no authentication required)
func isPublicUnary(method string) bool {
	return method == "/pb.Signaling/Health"
}

// isPublicStream checks if a stream method is public (no authentication required)
func isPublicStream(method string) bool {
	// by default require auth on streaming Signal. adjust if needed.
	return false
}
