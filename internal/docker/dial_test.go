package docker

import (
	"context"
	"testing"
	"time"
)

// The platform dialer is used; with or without a daemon it must return within the deadline.
func TestPlatformDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = New().Version(ctx)
}
