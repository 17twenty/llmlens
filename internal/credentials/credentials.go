package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
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

// Import installs a Bundle into a freshly-launched browser before any user
// navigation occurs. Cookies are set via Network.setCookies; per-origin storage
// is replayed by navigating to each origin and evaluating JS.
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
		if err := chromedp.Run(b.Ctx(), chromedp.Navigate(origin)); err != nil {
			return fmt.Errorf("navigate %s: %w", origin, err)
		}
		if err := setStorage(b.Ctx(), store); err != nil {
			return fmt.Errorf("set storage for %s: %w", origin, err)
		}
	}
	return nil
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
