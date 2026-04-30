package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	icdp "llmlens/internal/cdp"
	"llmlens/internal/credentials"
	"llmlens/internal/session"
	"llmlens/internal/tools"
)

// smoketest runs the LLMLens smoke scenarios end-to-end against a launched
// (headed by default) Chrome. Exit code 0 = pass, 1 = fail.
func main() {
	scenario := flag.String("scenario", "google-discovery", "scenario name: google-discovery | google-interactive | creds-roundtrip | snapshot-shape | gmail-triage | gmail-read-message | twitter-triage | maps-shape")
	headless := flag.Bool("headless", false, "run chrome headless")
	chromePath := flag.String("chrome", "", "override chrome executable")
	userDataDir := flag.String("user-data-dir", "", "persistent profile dir (helps avoid consent gates)")
	profilePath := flag.String("profile", "", "credential bundle to import (creds-roundtrip)")
	flag.Parse()

	ctx, cancel := signalContext()
	defer cancel()

	sess, err := session.New("runs")
	if err != nil {
		fail("session: %v", err)
	}
	logf("run-id=%s artifacts=%s", sess.RunID, sess.Root)

	browserOpts := icdp.Options{
		Mode:        icdp.ModeLaunch,
		Headless:    *headless,
		ChromePath:  *chromePath,
		UserDataDir: *userDataDir,
	}

	// Bundle-importing scenarios share the same launch hook.
	var afterLaunch func(*icdp.Browser) error
	if *scenario == "creds-roundtrip" || *scenario == "gmail-triage" || *scenario == "gmail-read-message" || *scenario == "twitter-triage" {
		if *profilePath == "" {
			fail("%s requires -profile=<path>", *scenario)
		}
		bundle, err := credentials.LoadBundle(*profilePath)
		if err != nil {
			fail("load profile: %v", err)
		}
		logf("bundle: domain=%s cookies=%d origins=%d captured=%s",
			bundle.Domain, len(bundle.Cookies), len(bundle.Origins),
			bundle.CapturedAt.Format(time.RFC3339))
		afterLaunch = func(b *icdp.Browser) error {
			return credentials.Import(ctx, b, bundle)
		}
	}

	engine := tools.New(ctx, browserOpts, afterLaunch, sess)
	defer engine.Close()

	// Smoketests fail-fast on launch errors — call EnsureBrowser explicitly.
	if _, err := engine.EnsureBrowser(); err != nil {
		fail("browser: %v", err)
	}

	switch *scenario {
	case "google-discovery":
		runGoogleDiscovery(engine)
	case "google-interactive":
		runGoogleInteractive(engine)
	case "creds-roundtrip":
		runCredsRoundtrip(engine)
	case "snapshot-shape":
		runSnapshotShape(engine)
	case "gmail-triage":
		runGmailTriage(engine)
	case "gmail-read-message":
		runGmailReadMessage(engine)
	case "twitter-triage":
		runTwitterTriage(engine)
	case "maps-shape":
		runMapsShape(engine)
	default:
		fail("unknown scenario %q", *scenario)
	}
}

// -- google-discovery: direct search URL, snapshot, assert content -----------

func runGoogleDiscovery(e *tools.Engine) {
	step("navigate to google search results for 'Nick Glynn'")
	must(e.Navigate("https://www.google.com/search?q=Nick+Glynn&hl=en"))

	step("wait for page to settle")
	if err := e.WaitFor("load", 15*time.Second); err != nil {
		logf("warn: load wait failed: %v", err)
	}
	time.Sleep(800 * time.Millisecond) // small additional settle for js-rendered results

	step("capture snapshot with markdown + artifacts")
	snap, err := e.Snapshot(tools.SnapshotOpts{
		IncludeMarkdown: true,
		IncludeHTML:     true,
		SaveArtifacts:   true,
	})
	must(err)
	logf("url=%s title=%q elements=%d", snap.URL, snap.Title, len(snap.Elements))

	if strings.Contains(strings.ToLower(snap.URL), "consent") {
		fail("blocked by consent gate at %s — rerun with -user-data-dir to persist consent", snap.URL)
	}

	body := strings.ToLower(snap.Markdown)
	if !strings.Contains(body, "nick glynn") {
		fail("expected 'Nick Glynn' in rendered markdown but it was absent (saved artifacts: %s)", e.SessionRoot())
	}

	logf("PASS google-discovery: 'Nick Glynn' present in result page")
}

// -- google-interactive: type into the search box, press enter -------------

func runGoogleInteractive(e *tools.Engine) {
	step("navigate to google.com")
	must(e.Navigate("https://www.google.com/?hl=en"))
	must(e.WaitFor("load", 15*time.Second))

	step("snapshot to discover search box")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, SaveArtifacts: true})
	must(err)
	logf("found %d elements", len(snap.Elements))

	var ref string
	for _, el := range snap.Elements {
		if (el.Role == "combobox" || el.Role == "searchbox" || el.Role == "textbox") &&
			(strings.Contains(strings.ToLower(el.Name), "search") || el.Name == "") {
			ref = el.Ref
			break
		}
	}
	if ref == "" {
		fail("could not find a search-like input on google homepage")
	}
	logf("targeting %s as search box", ref)

	step("type query and submit")
	must(e.Type(ref, "Nick Glynn", true))

	step("wait for results")
	must(e.WaitFor("url:search?q=", 15*time.Second))
	time.Sleep(800 * time.Millisecond)

	step("capture snapshot with markdown")
	final, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: true, IncludeHTML: true, SaveArtifacts: true})
	must(err)
	logf("results url=%s title=%q", final.URL, final.Title)

	if !strings.Contains(strings.ToLower(final.Markdown), "nick glynn") {
		fail("expected 'Nick Glynn' in rendered markdown of results page")
	}
	logf("PASS google-interactive: typed query, results page contains 'Nick Glynn'")
}

// -- creds-roundtrip: import bundle, verify auth'd /feed/ ------------------

func runCredsRoundtrip(e *tools.Engine) {
	// bundle was loaded + applied via afterLaunch in main(); engine.EnsureBrowser
	// has already returned, so cookies are in place before we navigate.
	step("navigate to https://www.linkedin.com/feed/")
	must(e.Navigate("https://www.linkedin.com/feed/"))

	step("wait for page load")
	if werr := e.WaitFor("load", 20*time.Second); werr != nil {
		logf("warn: load wait: %v", werr)
	}
	time.Sleep(1500 * time.Millisecond) // let JS-rendered feed settle

	step("snapshot to inspect resulting URL + nav")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: true, IncludeHTML: true, SaveArtifacts: true})
	must(err)
	logf("url=%s title=%q elements=%d", snap.URL, snap.Title, len(snap.Elements))

	url := strings.ToLower(snap.URL)
	switch {
	case strings.Contains(url, "/login") ||
		strings.Contains(url, "/uas/login") ||
		strings.Contains(url, "/authwall"):
		fail("AUTH FAILURE: bounced to login (%s) — cookies were not honoured. artifacts: %s",
			snap.URL, e.SessionRoot())

	case strings.Contains(url, "/checkpoint/"):
		fail("VERIFICATION CHALLENGE: LinkedIn flagged the new browser instance (%s). "+
			"This is the trigger that justifies the chrome extension path. artifacts: %s",
			snap.URL, e.SessionRoot())
	}

	// /feed/ check: look for two of the canonical authenticated nav items.
	body := strings.ToLower(snap.Markdown)
	authMarkers := []string{"my network", "messaging", "notifications", "/feed/follows", "in/"}
	hits := 0
	for _, m := range authMarkers {
		if strings.Contains(body, m) {
			hits++
		}
	}
	if hits < 2 {
		fail("AMBIGUOUS: URL is %s but only %d/%d auth markers present. inspect %s/www.linkedin.com/",
			snap.URL, hits, len(authMarkers), e.SessionRoot())
	}

	logf("PASS creds-roundtrip: authenticated /feed/ loaded with %d/%d auth markers", hits, len(authMarkers))
}

// -- gmail-triage: assert imported bundle yields authenticated inbox -------

func runGmailTriage(e *tools.Engine) {
	step("navigate to https://mail.google.com/mail/u/0/#inbox")
	must(e.Navigate("https://mail.google.com/mail/u/0/#inbox"))

	step("wait for load")
	if werr := e.WaitFor("load", 20*time.Second); werr != nil {
		logf("warn: load wait: %v", werr)
	}
	time.Sleep(1500 * time.Millisecond)

	step("snapshot the inbox")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, IncludeHTML: true, SaveArtifacts: true})
	must(err)
	logf("url=%s title=%q elements=%d auth_required=%v",
		snap.URL, snap.Title, len(snap.Elements), snap.AuthRequired)

	if snap.AuthRequired {
		fail("AUTH WALL HIT: %s — bundle isn't enough for Gmail. This is a documented PRD §3.3 trigger. artifacts: %s",
			snap.AuthHint, e.SessionRoot())
	}

	url := strings.ToLower(snap.URL)
	if !strings.Contains(url, "mail.google.com") {
		fail("expected to land on mail.google.com, got %s", snap.URL)
	}
	if strings.Contains(url, "/signin") || strings.Contains(url, "accounts.google.com") {
		fail("AUTH FAILURE: bounced to Google sign-in (%s) — bundle didn't carry. artifacts: %s",
			snap.URL, e.SessionRoot())
	}

	if len(snap.Elements) < 30 {
		fail("expected ≥30 elements on Gmail inbox (got %d) — page likely didn't fully render", len(snap.Elements))
	}

	// Inbox markers: "Compose" button (always present), "Inbox" link/text
	titleLower := strings.ToLower(snap.Title)
	if !strings.Contains(titleLower, "inbox") && !strings.Contains(titleLower, "mail") && !strings.Contains(titleLower, "gmail") {
		fail("title %q doesn't look like Gmail inbox", snap.Title)
	}

	// Count link-roled elements with name suggesting messages or nav
	composeFound := false
	for _, el := range snap.Elements {
		n := strings.ToLower(el.Name)
		if strings.Contains(n, "compose") {
			composeFound = true
		}
	}
	if !composeFound {
		logf("warn: no 'Compose' element seen — Gmail UI may have shifted. Inspect %s", e.SessionRoot())
	}

	logf("PASS gmail-triage: authenticated inbox loaded (%d elements, compose visible=%v)",
		len(snap.Elements), composeFound)
}

// -- maps-shape: assert vision_recommended fires on canvas-rendered pages

// Google Maps is the canonical canvas-rendered surface — the actual map and
// its annotations live on a full-viewport <canvas> that the AXTree never
// exposes. This scenario guards the canvas-detection probe in
// perception.detectVisionNeed: snapshot must set vision_recommended=true
// with a reason mentioning canvas. No auth required, fast.
func runMapsShape(e *tools.Engine) {
	step("navigate to https://maps.google.com")
	must(e.Navigate("https://maps.google.com"))

	step("wait for load + map render")
	if werr := e.WaitFor("load", 15*time.Second); werr != nil {
		logf("warn: load wait: %v", werr)
	}
	time.Sleep(2 * time.Second) // canvas paints after layout settles

	step("snapshot — assert vision_recommended fires")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, SaveArtifacts: true})
	must(err)
	logf("url=%s elements=%d vision_recommended=%v",
		snap.URL, len(snap.Elements), snap.VisionRecommended)
	logf("reason: %q", snap.VisionReason)

	if !snap.VisionRecommended {
		fail("expected vision_recommended=true on Maps (canvas surface), got false. inspect %s",
			e.SessionRoot())
	}
	if !strings.Contains(strings.ToLower(snap.VisionReason), "canvas") {
		fail("expected reason to mention canvas, got %q", snap.VisionReason)
	}

	logf("PASS maps-shape: vision hint fires on canvas-rendered page")
}

// -- twitter-triage: assert imported bundle yields authenticated x.com/home

func runTwitterTriage(e *tools.Engine) {
	step("navigate to https://x.com/home")
	must(e.Navigate("https://x.com/home"))

	step("wait for load")
	if werr := e.WaitFor("load", 20*time.Second); werr != nil {
		logf("warn: load wait: %v", werr)
	}
	time.Sleep(2 * time.Second) // X loads its feed in stages

	step("snapshot the home feed")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, IncludeHTML: true, SaveArtifacts: true})
	must(err)
	logf("url=%s title=%q elements=%d auth_required=%v",
		snap.URL, snap.Title, len(snap.Elements), snap.AuthRequired)

	if snap.AuthRequired {
		fail("AUTH WALL HIT: %s — bundle isn't enough for X. Likely PRD §3.3 trigger #2 (X aggressive bot detection). artifacts: %s",
			snap.AuthHint, e.SessionRoot())
	}

	url := strings.ToLower(snap.URL)
	if strings.Contains(url, "/i/flow/login") || strings.Contains(url, "/login") {
		fail("AUTH FAILURE: bounced to login flow (%s) — bundle didn't carry. artifacts: %s",
			snap.URL, e.SessionRoot())
	}
	if !strings.Contains(url, "x.com") && !strings.Contains(url, "twitter.com") {
		fail("expected to land on x.com, got %s", snap.URL)
	}

	if len(snap.Elements) < 30 {
		fail("expected ≥30 elements on X home (got %d) — page likely didn't fully render. inspect %s",
			len(snap.Elements), e.SessionRoot())
	}

	// Soft markers: Post button (compose), Home nav, "What is happening?!" placeholder
	postSeen, homeSeen, composeSeen := false, false, false
	for _, el := range snap.Elements {
		n := strings.ToLower(el.Name)
		if strings.Contains(n, "post") && el.Role == "button" {
			postSeen = true
		}
		if strings.Contains(n, "home") && (el.Role == "link" || el.Role == "tab") {
			homeSeen = true
		}
		if strings.Contains(n, "what is happening") || strings.Contains(n, "what's happening") {
			composeSeen = true
		}
	}
	hits := 0
	for _, ok := range []bool{postSeen, homeSeen, composeSeen} {
		if ok {
			hits++
		}
	}
	if hits < 1 {
		fail("expected at least one auth marker (Post button, Home nav, compose textarea) — found none. inspect %s",
			e.SessionRoot())
	}

	logf("PASS twitter-triage: authenticated x.com/home loaded (%d elements, markers: post=%v home=%v compose=%v)",
		len(snap.Elements), postSeen, homeSeen, composeSeen)
}

// -- gmail-read-message: open the first inbox email, assert iframe content visible

// Gmail renders message bodies inside a sandboxed sub-frame. Pre-iframe-traversal
// snapshot returns subjects + senders but no body content. This scenario opens
// the most recent email and asserts at least one snapshot Element has Frame!=""
// AND there's enough body-shaped content to plausibly read.
func runGmailReadMessage(e *tools.Engine) {
	step("navigate to https://mail.google.com/mail/u/0/#inbox")
	must(e.Navigate("https://mail.google.com/mail/u/0/#inbox"))

	step("wait for inbox to render")
	if werr := e.WaitFor("load", 20*time.Second); werr != nil {
		logf("warn: load wait: %v", werr)
	}
	time.Sleep(1500 * time.Millisecond)

	step("snapshot inbox to find an email row to open")
	inbox, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, SaveArtifacts: true})
	must(err)
	logf("inbox: url=%s elements=%d auth_required=%v",
		inbox.URL, len(inbox.Elements), inbox.AuthRequired)
	if inbox.AuthRequired {
		fail("auth wall on inbox: %s", inbox.AuthHint)
	}

	// Find the first link element whose name looks like an email row. Gmail
	// historically emits role=link with the subject in the name, but the
	// underlying tr.zA carries a richer structure. We pick the first link
	// whose href contains "#inbox/" — clicking it opens the message.
	var rowRef string
	for _, el := range inbox.Elements {
		if el.Role == "link" && strings.Contains(el.Href, "#inbox/") && rowRef == "" {
			rowRef = el.Ref
			logf("opening message: ref=%s name=%q href=%s",
				el.Ref, truncStr(el.Name, 80), el.Href)
		}
	}
	if rowRef == "" {
		// Fallback: first focusable thing whose name looks email-shaped
		for _, el := range inbox.Elements {
			if el.Focusable && len(el.Name) > 30 && rowRef == "" {
				rowRef = el.Ref
				logf("opening (fallback) message: ref=%s name=%q",
					el.Ref, truncStr(el.Name, 80))
			}
		}
	}
	if rowRef == "" {
		fail("no email row found in inbox snapshot — Gmail UI may have shifted. inspect %s",
			e.SessionRoot())
	}

	step("click the row to open the message")
	must(e.Click(rowRef))

	step("wait for message detail to settle")
	time.Sleep(2 * time.Second)

	step("snapshot the message detail view")
	detail, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, SaveArtifacts: true})
	must(err)

	frameCount := 0
	frameElems := 0
	uniqueFrames := map[string]int{}
	for _, el := range detail.Elements {
		if el.Frame != "" {
			frameCount++
			uniqueFrames[el.Frame]++
		}
	}
	frameElems = frameCount
	logf("detail: url=%s elements=%d (frame-attributed=%d, distinct frames=%d)",
		detail.URL, len(detail.Elements), frameElems, len(uniqueFrames))

	if len(uniqueFrames) == 0 {
		fail("expected ≥1 sub-frame in message detail view, found 0 — iframe traversal regressed. artifacts: %s",
			e.SessionRoot())
	}
	if frameCount < 2 {
		fail("only %d frame-attributed elements — iframe(s) may exist but be empty. artifacts: %s",
			frameCount, e.SessionRoot())
	}

	logf("PASS gmail-read-message: %d frames contributed %d elements (sub-frame perception live)",
		len(uniqueFrames), frameCount)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// -- snapshot-shape: assert snapshot returns expected fields ---------------

// example.com is the canonical lightest-weight fixture page: one heading,
// one paragraph, one link to iana.org. Stable for years. If snapshot's shape
// breaks, this scenario surfaces it cheaply with no auth or real network
// scraping.
func runSnapshotShape(e *tools.Engine) {
	step("navigate to https://example.com")
	must(e.Navigate("https://example.com"))

	step("wait for load")
	must(e.WaitFor("load", 10*time.Second))

	step("snapshot — assert shape (role=link element with non-empty href)")
	snap, err := e.Snapshot(tools.SnapshotOpts{IncludeMarkdown: false, SaveArtifacts: true})
	must(err)

	if len(snap.Elements) < 2 {
		fail("expected at least 2 elements on example.com, got %d", len(snap.Elements))
	}

	var foundLinkWithHref bool
	for _, el := range snap.Elements {
		if el.Role == "link" {
			if el.Href == "" {
				fail("link element ref=%s name=%q has empty Href — hydrateLinkHrefs regressed",
					el.Ref, el.Name)
			}
			if !strings.HasPrefix(el.Href, "https://") && !strings.HasPrefix(el.Href, "http://") {
				fail("link element ref=%s href=%q is not a usable URL",
					el.Ref, el.Href)
			}
			foundLinkWithHref = true
			logf("link: ref=%s name=%q href=%s", el.Ref, el.Name, el.Href)
		}
	}
	if !foundLinkWithHref {
		fail("expected at least one role=link element on example.com, found none (%d total)", len(snap.Elements))
	}

	logf("PASS snapshot-shape: %d elements, link role with href present", len(snap.Elements))
}

// --- helpers --------------------------------------------------------------

func step(msg string)         { fmt.Fprintf(os.Stderr, "→ %s\n", msg) }
func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "  "+f+"\n", a...) }
func must(err error) {
	if err != nil {
		fail("error: %v", err)
	}
}
func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL "+f+"\n", a...)
	os.Exit(1)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
