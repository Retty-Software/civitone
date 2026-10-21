//go:build !windows

package main

import (
	"context"

	"github.com/Retty-Software/civitone/controller/internal/srcapp"
)

// runUI has no tray off Windows yet: it waits until ctx ends.
func runUI(ctx context.Context, url string, app *srcapp.App, quit func()) {
	<-ctx.Done()
}
