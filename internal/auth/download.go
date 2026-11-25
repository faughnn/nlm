package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// ownedDownloadTimeout bounds one browser download, start to finish.
const ownedDownloadTimeout = 5 * time.Minute

// ErrOwnedProfileSignedOut reports that nlm's own browser profile has no
// Google session, so a download through it lands on the sign-in page.
var ErrOwnedProfileSignedOut = errors.New("nlm's browser profile is not signed in to Google; run `nlm auth login -profile nlm` in a terminal")

// DownloadWithOwnedProfile saves fileURL to destPath through a headless
// browser running on nlm's own persistent profile, and returns the bytes
// written. Google's media CDN accepts a signed-in session only from the
// browser that holds it: the same cookies replayed by an HTTP client are
// redirected to sign-in. The profile is used in place, never copied, and
// with the launch options of the login that wrote it.
func (ba *BrowserAuth) DownloadWithOwnedProfile(ctx context.Context, fileURL, userDataDir, destPath string) (int64, error) {
	if _, err := os.Stat(userDataDir); err != nil {
		return 0, fmt.Errorf("nlm browser profile %s: %w", userDataDir, err)
	}
	// The owned-profile login resolves its browser the same way.
	browserPath := getBrowserPathForProfile("Brave")
	if browserPath == "" {
		return 0, fmt.Errorf("could not find browser")
	}
	downloadDir, err := os.MkdirTemp("", "nlm-download-*")
	if err != nil {
		return 0, fmt.Errorf("create download dir: %w", err)
	}
	defer os.RemoveAll(downloadDir)

	// Always headless: a download needs no window, and -debug should not open one.
	opts := browserExecOptions(browserPath, userDataDir, true, true, "0")
	if ba.debug {
		opts = append(opts, chromedp.CombinedOutput(browserDebugWriter{}))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()
	bctx, cancel := newChromeContext(allocCtx, ba.debug)
	defer cancel()
	bctx, cancel = context.WithTimeout(bctx, ownedDownloadTimeout)
	defer cancel()

	watcher := newDownloadWatcher()
	watcher.logf = ba.debugf
	// Download events go to the session that enabled them: this page's target,
	// since SetDownloadBehavior below is sent through it.
	chromedp.ListenTarget(bctx, watcher.handle)
	if err := chromedp.Run(bctx, browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
		WithDownloadPath(downloadDir).
		WithEventsEnabled(true)); err != nil {
		return 0, fmt.Errorf("enable browser downloads: %w", err)
	}

	ba.debugf("downloading %s through %s", redactQuery(fileURL), userDataDir)
	var finalURL string
	navErr := chromedp.Run(bctx, chromedp.Navigate(fileURL), chromedp.Location(&finalURL))
	ba.debugf("navigation finished: err=%v page=%s download started=%v", navErr, redactQuery(finalURL), watcher.started())
	switch {
	case navErr != nil && !isNavigationAbort(navErr):
		return 0, fmt.Errorf("open download URL: %w", navErr)
	case navErr == nil && !watcher.started():
		// A page loaded instead of a download starting.
		if landedOnSignIn(finalURL) {
			return 0, ErrOwnedProfileSignedOut
		}
		return 0, fmt.Errorf("the URL opened as a page (%s), not a download", redactQuery(finalURL))
	}
	// Chrome aborts a navigation that turns into a download; the download goes on.
	outcome, err := watcher.wait(bctx)
	if err != nil {
		return 0, err
	}
	return finishDownload(filepath.Join(downloadDir, outcome.guid), destPath, outcome.total)
}

// downloadWatcher records the one download a navigation starts.
type downloadWatcher struct {
	mu    sync.Mutex
	begun bool
	done  chan downloadOutcome
	logf  func(format string, args ...interface{})
}

type downloadOutcome struct {
	guid  string
	total int64
	err   error
}

func newDownloadWatcher() *downloadWatcher {
	return &downloadWatcher{done: make(chan downloadOutcome, 1)}
}

func (w *downloadWatcher) handle(ev interface{}) {
	switch e := ev.(type) {
	case *browser.EventDownloadWillBegin:
		w.mu.Lock()
		w.begun = true
		w.mu.Unlock()
		w.log("download began: %s (suggested name %q)", redactQuery(e.URL), e.SuggestedFilename)
	case *browser.EventDownloadProgress:
		if e.State != browser.DownloadProgressStateInProgress {
			w.log("download %s: %.0f of %.0f bytes", e.State, e.ReceivedBytes, e.TotalBytes)
		}
		switch e.State {
		case browser.DownloadProgressStateCompleted:
			w.finish(downloadOutcome{guid: e.GUID, total: int64(e.TotalBytes)})
		case browser.DownloadProgressStateCanceled:
			w.finish(downloadOutcome{guid: e.GUID, err: errors.New("the browser canceled the download")})
		}
	}
}

func (w *downloadWatcher) log(format string, args ...interface{}) {
	if w.logf != nil {
		w.logf(format, args...)
	}
}

func (w *downloadWatcher) finish(o downloadOutcome) {
	select {
	case w.done <- o:
	default:
	}
}

func (w *downloadWatcher) started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.begun
}

func (w *downloadWatcher) wait(ctx context.Context) (downloadOutcome, error) {
	select {
	case o := <-w.done:
		if o.err != nil {
			return o, o.err
		}
		return o, nil
	case <-ctx.Done():
		return downloadOutcome{}, fmt.Errorf("download did not finish: %w", ctx.Err())
	}
}

// finishDownload moves a completed browser download to destPath after
// checking it is the whole file and not a web page. A cookie-less or
// signed-out CDN request can answer 200 with an HTML page or a short body.
func finishDownload(src, destPath string, total int64) (int64, error) {
	info, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("find downloaded file: %w", err)
	}
	if total > 0 && info.Size() != total {
		return 0, fmt.Errorf("download incomplete: %d of %d bytes", info.Size(), total)
	}
	if isHTMLFile(src) {
		return 0, fmt.Errorf("download is a web page, not media: %w", ErrOwnedProfileSignedOut)
	}
	if err := moveFile(src, destPath); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func isHTMLFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return strings.HasPrefix(http.DetectContentType(head[:n]), "text/html")
}

// moveFile renames src to dst, copying when they sit on different volumes.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open download: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return out.Close()
}

func isNavigationAbort(err error) bool {
	return err != nil && strings.Contains(err.Error(), "net::ERR_ABORTED")
}

func landedOnSignIn(pageURL string) bool {
	u, err := url.Parse(pageURL)
	return err == nil && u.Hostname() == "accounts.google.com"
}

// redactQuery drops a signed URL's query and shortens its path for logs.
func redactQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	path := u.Path
	if len(path) > 24 {
		path = path[:24] + "…"
	}
	return u.Scheme + "://" + u.Host + path
}
