package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tmc/nlm/internal/auth"
	"github.com/tmc/nlm/nlmauth"
	"github.com/tmc/nlm/notebooklm"
)

// downloadAudioWithOwnedBrowser saves a notebook's audio overview through
// nlm's own signed-in browser profile and returns the bytes written. Google's
// media CDN redirects the stored session cookies to sign-in when an HTTP
// client replays them, but serves the same request from the browser that
// holds the session. It is a variable so tests can stand in for the browser.
var downloadAudioWithOwnedBrowser = func(c *notebooklm.Client, notebookID, filename string) (int64, error) {
	ctx := context.Background()
	audioURL, err := c.AudioOverviewDownloadURL(ctx, notebookID)
	if err != nil {
		return 0, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("locate browser profile: %w", err)
	}
	profileDir := filepath.Join(home, ".nlm", "browser", firstNonEmpty(activeIdentity, nlmauth.DefaultIdentity))
	return auth.New(debug).DownloadWithOwnedProfile(ctx, audioURL, profileDir, filename)
}
