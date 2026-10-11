package bucket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

type accountContextKey struct{}

// WithAccount binds admission and subsequent usage to a hashed account identity.
// A context without an account uses the shared global bucket.
func WithAccount(ctx context.Context, identifier string) context.Context {
	digest := sha256.Sum256([]byte(identifier))
	return context.WithValue(ctx, accountContextKey{}, hex.EncodeToString(digest[:]))
}

// AccountKey returns the hashed identity, or an empty string for the global bucket.
func AccountKey(ctx context.Context) string {
	key, _ := ctx.Value(accountContextKey{}).(string)
	return key
}
