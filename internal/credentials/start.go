package credentials

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/chromedp"

	"llmlens/internal/auth"
	icdp "llmlens/internal/cdp"
)

// StartOptions configures the human-in-the-loop login capture flow.
type StartOptions struct {
	// Domain is the apex to navigate to and to scope cookies on. e.g. "linkedin.com".
	Domain string

	// SettleTime is how long the URL must remain stable on a non-login page
	// before we trust that login completed. Default 3s.
	SettleTime time.Duration

	// Timeout caps the total wait. Default 5 minutes.
	Timeout time.Duration

	// KeepOpen leaves the browser running after capture. Default false.
	KeepOpen bool

	// OnLog receives progress messages — wire to stderr from a CLI.
	OnLog func(string)
}

// Start launches a dedicated, headed Chrome to https://<domain>/, watches the
// main-frame URL for the user to finish logging in, then captures cookies
// and origin storage. The browser is closed unless KeepOpen is set.
//
// "Login complete" is detected when the URL no longer matches any common
// login/checkpoint/auth pattern AND has stayed stable for SettleTime. This
// reliably catches both fresh-login flows (… → /login → /feed/) and
// already-logged-in profiles (→ /feed/ on first nav).
func Start(ctx context.Context, opts StartOptions) (*Bundle, error) {
	if opts.Domain == "" {
		return nil, fmt.Errorf("auth start: domain is required")
	}
	if opts.SettleTime == 0 {
		opts.SettleTime = 3 * time.Second
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.OnLog == nil {
		opts.OnLog = func(string) {}
	}

	tmpDir, err := os.MkdirTemp("", "llmlens-authstart-*")
	if err != nil {
		return nil, fmt.Errorf("auth start: temp profile dir: %w", err)
	}
	if !opts.KeepOpen {
		defer os.RemoveAll(tmpDir)
	}
	opts.OnLog(fmt.Sprintf("ephemeral profile dir: %s", tmpDir))

	timeoutCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	b, err := icdp.New(timeoutCtx, icdp.Options{
		Mode:        icdp.ModeLaunch,
		Headless:    false,
		UserDataDir: tmpDir,
	})
	if err != nil {
		return nil, fmt.Errorf("auth start: launch chrome: %w", err)
	}
	closeBrowser := func() {
		if !opts.KeepOpen {
			b.Close()
		}
	}

	target := "https://" + opts.Domain + "/"
	opts.OnLog(fmt.Sprintf("navigating to %s — log in in the window that opened", target))
	if err := chromedp.Run(b.Ctx(), chromedp.Navigate(target)); err != nil {
		closeBrowser()
		return nil, fmt.Errorf("auth start: navigate: %w", err)
	}

	opts.OnLog(fmt.Sprintf("waiting for login — capture fires after %s of stable, non-login URL (timeout %s)",
		opts.SettleTime, opts.Timeout))

	currentURL := ""
	lastChange := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			closeBrowser()
			if timeoutCtx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("auth start: timed out after %s (last URL: %s)", opts.Timeout, currentURL)
			}
			return nil, timeoutCtx.Err()
		case <-ticker.C:
			var u string
			if err := chromedp.Run(b.Ctx(), chromedp.Location(&u)); err != nil {
				closeBrowser()
				return nil, fmt.Errorf("auth start: browser session ended: %w", err)
			}
			if u != currentURL {
				currentURL = u
				lastChange = time.Now()
				opts.OnLog(fmt.Sprintf("URL → %s", u))
			}
			if isLoginLike(u) {
				continue
			}
			if time.Since(lastChange) < opts.SettleTime {
				continue
			}
			opts.OnLog(fmt.Sprintf("settled on %s — capturing", u))
			// Start's temp profile is naturally scoped — capture all cookies
			// it accumulated during login, including cross-domain auth state
			// (e.g. accounts.google.com cookies for a mail.google.com session).
			bundle, capErr := capture(b, opts.Domain, false)
			if !opts.KeepOpen {
				b.Close()
			}
			return bundle, capErr
		}
	}
}

// isLoginLike is now a thin alias for auth.IsLoginURL — kept as a private
// re-export so call sites in this file stay readable.
func isLoginLike(u string) bool { return auth.IsLoginURL(u) }
