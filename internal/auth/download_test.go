package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
)

func TestDownloadWatcherCompleted(t *testing.T) {
	w := newDownloadWatcher()
	if w.started() {
		t.Fatal("started before any event")
	}
	w.handle(&browser.EventDownloadWillBegin{GUID: "g1"})
	if !w.started() {
		t.Fatal("not started after downloadWillBegin")
	}
	w.handle(&browser.EventDownloadProgress{GUID: "g1", State: browser.DownloadProgressStateInProgress, ReceivedBytes: 10, TotalBytes: 100})
	w.handle(&browser.EventDownloadProgress{GUID: "g1", State: browser.DownloadProgressStateCompleted, ReceivedBytes: 100, TotalBytes: 100})
	o, err := w.wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if o.guid != "g1" || o.total != 100 {
		t.Fatalf("outcome = %+v, want guid g1 total 100", o)
	}
}

func TestDownloadWatcherCanceled(t *testing.T) {
	w := newDownloadWatcher()
	w.handle(&browser.EventDownloadProgress{GUID: "g1", State: browser.DownloadProgressStateCanceled})
	if _, err := w.wait(context.Background()); err == nil {
		t.Fatal("canceled download returned no error")
	}
}

func TestDownloadWatcherTimesOut(t *testing.T) {
	w := newDownloadWatcher()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
}

func TestFinishDownload(t *testing.T) {
	media := append([]byte("\x00\x00\x00\x20ftypM4A "), make([]byte, 600)...)
	tests := []struct {
		name    string
		body    []byte
		total   int64
		wantErr string
	}{
		{name: "complete media", body: media, total: int64(len(media))},
		{name: "unknown total", body: media, total: 0},
		{name: "short body", body: media[:100], total: int64(len(media)), wantErr: "incomplete"},
		{name: "sign-in page", body: []byte("<!DOCTYPE html><html><body>Sign in</body></html>"), wantErr: "web page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "guid")
			dst := filepath.Join(dir, "out.m4a")
			if err := os.WriteFile(src, tt.body, 0o600); err != nil {
				t.Fatal(err)
			}
			n, err := finishDownload(src, dst, tt.total)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if _, statErr := os.Stat(dst); statErr == nil {
					t.Fatal("rejected download was written to the destination")
				}
				return
			}
			if err != nil {
				t.Fatalf("finishDownload: %v", err)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(tt.body)) || len(got) != len(tt.body) {
				t.Fatalf("wrote %d bytes (returned %d), want %d", len(got), n, len(tt.body))
			}
		})
	}
}

func TestLandedOnSignIn(t *testing.T) {
	if !landedOnSignIn("https://accounts.google.com/v3/signin/identifier?continue=x") {
		t.Error("sign-in page not detected")
	}
	if landedOnSignIn("https://lh3.googleusercontent.com/abc=m140-dv") {
		t.Error("media URL reported as sign-in")
	}
}

func TestIsNavigationAbort(t *testing.T) {
	if !isNavigationAbort(errors.New("page load error net::ERR_ABORTED")) {
		t.Error("ERR_ABORTED not recognized")
	}
	if isNavigationAbort(errors.New("page load error net::ERR_NAME_NOT_RESOLVED")) || isNavigationAbort(nil) {
		t.Error("other errors recognized as an abort")
	}
}

func TestRedactQueryDropsSignature(t *testing.T) {
	got := redactQuery("https://lh3.googleusercontent.com/notebooklm/AVeryLongSignedPathSegment123=m140-dv?sig=secret")
	if strings.Contains(got, "secret") || strings.Contains(got, "?") {
		t.Fatalf("redactQuery leaked the query: %s", got)
	}
	if !strings.HasPrefix(got, "https://lh3.googleusercontent.com/") {
		t.Fatalf("redactQuery lost the host: %s", got)
	}
}
