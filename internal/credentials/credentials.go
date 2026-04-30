package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	icdp "llmlens/internal/cdp"
)

// Bundle is a serialisable auth profile: cookies plus per-origin web storage.
type Bundle struct {
	Domain     string                   `json:"domain"`
	CapturedAt time.Time                `json:"captured_at"`
	Cookies    []*network.Cookie        `json:"cookies"`
	Origins    map[string]OriginStorage `json:"origins,omitempty"`
}

type OriginStorage struct {
	Local   map[string]string `json:"local,omitempty"`
	Session map[string]string `json:"session,omitempty"`
}

// Export attaches to a running browser, opens (or reuses) a tab on
// https://<domain>, captures cookies + per-origin storage, and returns a Bundle.
//
// The browser must already be running with a CDP endpoint (e.g. Chrome started
// with --remote-debugging-port=9222) and be logged in to the target domain.
func Export(ctx context.Context, remoteURL, domain string) (*Bundle, error) {
	b, err := icdp.New(ctx, icdp.Options{Mode: icdp.ModeAttach, RemoteURL: remoteURL})
	if err != nil {
		return nil, err
	}
	defer b.Close()

	target := "https://" + domain + "/"
	if err := chromedp.Run(b.Ctx(), chromedp.Navigate(target)); err != nil {
		return nil, fmt.Errorf("navigate to %s: %w", target, err)
	}
	// Export attaches to the user's daily Chrome — filter cookies to the
	// requested domain so we don't slurp up unrelated session state.
	return capture(b, domain, true)
}

// capture is the shared cookie + storage harvester. filterByDomain controls
// whether cookies are scoped to the requested apex (Export's privacy story
// against an attached daily Chrome) or captured wholesale (Start's flow,
// which uses a fresh ephemeral profile that only contains login-flow
// cookies — typically across multiple domains for federated auth like
// Google's accounts.google.com → mail.google.com handoff).
func capture(b *icdp.Browser, domain string, filterByDomain bool) (*Bundle, error) {
	bundle := &Bundle{
		Domain:     domain,
		CapturedAt: time.Now().UTC(),
		Origins:    map[string]OriginStorage{},
	}
	if err := chromedp.Run(b.Ctx(), chromedp.ActionFunc(func(ctx context.Context) error {
		all, err := network.GetCookies().Do(ctx)
		if err != nil {
			return err
		}
		if filterByDomain {
			bundle.Cookies = filterCookies(all, domain)
		} else {
			bundle.Cookies = all
		}
		return nil
	})); err != nil {
		return nil, fmt.Errorf("get cookies: %w", err)
	}

	var currentURL string
	_ = chromedp.Run(b.Ctx(), chromedp.Location(&currentURL))
	if currentURL == "" {
		currentURL = "https://" + domain + "/"
	}
	if store, err := dumpOriginStorage(b.Ctx()); err == nil {
		bundle.Origins[originOf(currentURL)] = store
	}
	return bundle, nil
}

// originOf returns scheme://host[:port] for the URL, the form expected when
// replaying storage on import. Falls back to the input on parse failure.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host + "/"
}

// Import installs cookies eagerly and arms one-shot listeners for per-origin
// storage replay.
//
// Cookies are domain-scoped — Network.SetCookies works without any
// navigation. localStorage / sessionStorage are origin-scoped; CDP can only
// write them from a document loaded on that origin, which means a real
// navigation is required.
//
// An earlier version navigated to every captured origin during Import,
// which loaded 2–3 real sites at browser launch (slow, surprising,
// detection-shaped — three sequential cold requests with no human-like
// pacing is a classic bot signature).
//
// The lazy approach: cookies eagerly, then a chromedp listener per origin
// that fires storage replay the first time the agent's main frame
// navigates to a matching origin. sync.Once guarantees a single replay
// per origin per Import call.
//
// Race window: between EventFrameNavigated and storage injection
// completing, a CDP command from the agent could observe the page
// pre-injection. In practice agents do navigate → wait_for(load) →
// snapshot, giving ~hundreds of ms for the goroutine spawned from the
// listener to finish — comfortable. If a future site reads localStorage
// during initial render before our injection lands, we'll need to
// either pre-stamp via Page.addScriptToEvaluateOnNewDocument or wrap
// navigate to await pending injections.
func Import(ctx context.Context, b *icdp.Browser, bundle *Bundle) error {
	if len(bundle.Cookies) == 0 && len(bundle.Origins) == 0 {
		return nil
	}
	if err := chromedp.Run(b.Ctx(), chromedp.ActionFunc(func(ctx context.Context) error {
		params := make([]*network.CookieParam, 0, len(bundle.Cookies))
		for _, c := range bundle.Cookies {
			params = append(params, &network.CookieParam{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   c.Domain,
				Path:     c.Path,
				Secure:   c.Secure,
				HTTPOnly: c.HTTPOnly,
				SameSite: c.SameSite,
				Expires:  cdpExpires(c.Expires),
			})
		}
		return network.SetCookies(params).Do(ctx)
	})); err != nil {
		return fmt.Errorf("set cookies: %w", err)
	}

	for origin, store := range bundle.Origins {
		armStorageReplay(b.Ctx(), origin, store)
	}
	return nil
}

// armStorageReplay installs a chromedp listener that fires storage
// replay the first time the agent's main frame navigates to the given
// origin. Idempotent via sync.Once.
//
// Listeners run on chromedp's event-delivery goroutine, which means they
// must NOT call chromedp.Run synchronously (it'd deadlock waiting for an
// event the same loop should be delivering). We spawn a goroutine for
// the actual injection; chromedp.Run inside it works fine.
func armStorageReplay(ctx context.Context, origin string, store OriginStorage) {
	var once sync.Once
	chromedp.ListenTarget(ctx, func(ev any) {
		e, ok := ev.(*page.EventFrameNavigated)
		if !ok || e.Frame == nil || e.Frame.ParentID != "" {
			return
		}
		if originOf(e.Frame.URL) != origin {
			return
		}
		once.Do(func() {
			go func() {
				// Best-effort: storage replay failure shouldn't crash
				// the agent flow. The cookies got us here; storage is a
				// nice-to-have for the typical site.
				_ = setStorage(ctx, store)
			}()
		})
	})
}

func dumpOriginStorage(ctx context.Context) (OriginStorage, error) {
	js := `(() => {
		const dump = (s) => {
			const out = {};
			for (let i = 0; i < s.length; i++) {
				const k = s.key(i);
				out[k] = s.getItem(k);
			}
			return out;
		};
		return { local: dump(localStorage), session: dump(sessionStorage) };
	})()`
	var raw struct {
		Local   map[string]string `json:"local"`
		Session map[string]string `json:"session"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw)); err != nil {
		return OriginStorage{}, err
	}
	return OriginStorage{Local: raw.Local, Session: raw.Session}, nil
}

func setStorage(ctx context.Context, s OriginStorage) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	js := fmt.Sprintf(`(() => {
		const data = %s;
		for (const [k,v] of Object.entries(data.local || {})) localStorage.setItem(k,v);
		for (const [k,v] of Object.entries(data.session || {})) sessionStorage.setItem(k,v);
		return true;
	})()`, payload)
	var ok bool
	return chromedp.Run(ctx, chromedp.Evaluate(js, &ok))
}

// filterCookies keeps cookies whose domain matches the requested apex domain.
// CDP returns leading-dot domains for "host-spanning" cookies; we accept both.
func filterCookies(all []*network.Cookie, domain string) []*network.Cookie {
	d := strings.TrimPrefix(strings.ToLower(domain), ".")
	out := all[:0:0]
	for _, c := range all {
		cd := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if cd == d || strings.HasSuffix(cd, "."+d) {
			out = append(out, c)
		}
	}
	return out
}

func cdpExpires(secs float64) *cdp.TimeSinceEpoch {
	if secs <= 0 {
		return nil
	}
	t := cdp.TimeSinceEpoch(time.Unix(int64(secs), 0))
	return &t
}

func SaveBundle(b *Bundle, path string) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func LoadBundle(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}
