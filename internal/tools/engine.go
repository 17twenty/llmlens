package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpdom "github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	icdp "llmlens/internal/cdp"
	"llmlens/internal/perception"
	"llmlens/internal/session"
)

// Engine wires the browser, the active session, and the latest snapshot ref map
// into a single dispatch surface for the tool API.
//
// The browser is launched lazily on the first tool call that needs it
// (anything except metadata-only RPC like initialize/tools-list). This keeps
// MCP server registration cheap — registering llmlens with a Claude Code
// project no longer spawns a visible Chrome window per conversation.
type Engine struct {
	session *session.Session

	// lazy browser construction
	parentCtx     context.Context
	browserOpts   icdp.Options
	onAfterLaunch func(*icdp.Browser) error // optional hook (e.g. import profile)

	bMu     sync.Mutex
	browser *icdp.Browser

	mu         sync.Mutex
	lastRefMap map[string]int64
}

// New builds an Engine with a lazy-launched browser. opts and onAfterLaunch
// are remembered until the first tool call that touches the browser. Pass
// onAfterLaunch=nil if no post-launch setup is needed.
func New(parent context.Context, opts icdp.Options, onAfterLaunch func(*icdp.Browser) error, s *session.Session) *Engine {
	return &Engine{
		parentCtx:     parent,
		browserOpts:   opts,
		onAfterLaunch: onAfterLaunch,
		session:       s,
		lastRefMap:    map[string]int64{},
	}
}

// EnsureBrowser launches the browser if it hasn't been already. Idempotent.
// Most callers don't need to invoke this directly — every public tool method
// calls it implicitly. Smoketests / scripted callers may call it explicitly
// to fail-fast on launch errors.
//
// Self-healing: if the cached browser's chromedp context has been cancelled
// (Chrome process died, websocket closed, unrecoverable CDP error tore the
// context down), the cached *Browser is discarded and a fresh one is
// launched. Without this check, a single Chrome death would cause every
// subsequent tool call to return "context canceled" until the MCP server
// itself restarted.
func (e *Engine) EnsureBrowser() (*icdp.Browser, error) {
	e.bMu.Lock()
	defer e.bMu.Unlock()
	if e.browser != nil {
		if e.browser.Ctx().Err() == nil {
			return e.browser, nil
		}
		// Browser context is dead — clean up and fall through to relaunch.
		e.browser.Close()
		e.browser = nil
	}
	b, err := icdp.New(e.parentCtx, e.browserOpts)
	if err != nil {
		return nil, wrap(err)
	}
	if e.onAfterLaunch != nil {
		if hookErr := e.onAfterLaunch(b); hookErr != nil {
			b.Close()
			return nil, wrap(hookErr)
		}
	}
	e.browser = b
	return b, nil
}

// CloseBrowser is the agent-callable shutdown — the underlying close path is
// the same as the engine-cleanup Close(), but we record it as a tool call
// in events.log so it shows up in tool-frequency triage. The next tool call
// that needs a browser will lazy-relaunch via EnsureBrowser.
func (e *Engine) CloseBrowser() (err error) {
	defer e.record("close_browser", nil)(&err)
	e.Close()
	return nil
}

// Close shuts down the browser if one was launched. Safe to call when no
// browser was ever needed.
func (e *Engine) Close() {
	e.bMu.Lock()
	defer e.bMu.Unlock()
	if e.browser != nil {
		e.browser.Close()
		e.browser = nil
	}
}

// ctx returns a usable context for chromedp calls. Callers must have
// previously run EnsureBrowser successfully — it's cheap to call defensively.
func (e *Engine) ctx() context.Context {
	e.bMu.Lock()
	defer e.bMu.Unlock()
	return e.browser.Ctx()
}

// SessionRoot returns the on-disk artifact directory for this run, or "" if
// no session was attached.
func (e *Engine) SessionRoot() string {
	if e.session == nil {
		return ""
	}
	return e.session.Root
}

// record returns a closer that logs the tool call to events.log on the
// associated session. Pattern: `defer e.record(tool, params)(&err)` — the
// pointer is captured at defer time, dereferenced at the deferred call so
// the final error value is what gets logged.
func (e *Engine) record(tool string, params any) func(*error) {
	start := time.Now()
	return func(errp *error) {
		if e.session == nil {
			return
		}
		var err error
		if errp != nil {
			err = *errp
		}
		ev := session.Event{
			Timestamp:  start.UTC(),
			Tool:       tool,
			Params:     truncateParams(params),
			DurationMS: time.Since(start).Milliseconds(),
		}
		if err != nil {
			ev.OK = false
			ev.Error = &session.EventError{
				Category: errorCategory(err),
				// Error messages can carry stack traces (eval_threw) — keep
				// 4000 chars so failures stay debuggable. Params remain at
				// 500; we don't need full input echo.
				Message: truncate(err.Error(), 4000),
			}
		} else {
			ev.OK = true
		}
		_ = e.session.AppendEvent(ev)
	}
}

func errorCategory(err error) string {
	var te *Error
	if errors.As(err, &te) {
		return string(te.Code)
	}
	return string(CodeInternal)
}

// truncateParams keeps log size bounded. Strings longer than 500 chars are
// trimmed; everything else passes through and gets JSON-marshalled by Go.
func truncateParams(p any) any {
	if p == nil {
		return nil
	}
	m, ok := p.(map[string]any)
	if !ok {
		return p
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = truncate(s, 500)
		} else {
			out[k] = v
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// --- navigate -------------------------------------------------------------

func (e *Engine) Navigate(rawURL string) (err error) {
	defer e.record("navigate", map[string]any{"url": rawURL})(&err)
	if rawURL == "" {
		return newErr(CodeInvalidParam, "url required")
	}
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	if rerr := chromedp.Run(b.Ctx(), chromedp.Navigate(rawURL)); rerr != nil {
		w := wrap(rerr)
		if w.Code == CodeInternal {
			w.Code = CodeNavigationFailed
		}
		return w
	}
	return nil
}

func (e *Engine) Back() (err error) {
	defer e.record("back", nil)(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	return wrapNil(chromedp.Run(b.Ctx(), chromedp.NavigateBack()))
}
func (e *Engine) Forward() (err error) {
	defer e.record("forward", nil)(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	return wrapNil(chromedp.Run(b.Ctx(), chromedp.NavigateForward()))
}
func (e *Engine) Reload() (err error) {
	defer e.record("reload", nil)(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	return wrapNil(chromedp.Run(b.Ctx(), chromedp.Reload()))
}

func wrapNil(err error) error {
	if err == nil {
		return nil
	}
	return wrap(err)
}

// --- snapshot -------------------------------------------------------------

type SnapshotOpts struct {
	IncludeHTML     bool `json:"include_html,omitempty"`
	IncludeMarkdown bool `json:"include_markdown,omitempty"`
	SaveArtifacts   bool `json:"save_artifacts,omitempty"`
}

func (e *Engine) Snapshot(opts SnapshotOpts) (snap *perception.Snapshot, err error) {
	defer e.record("snapshot", map[string]any{
		"include_html":     opts.IncludeHTML,
		"include_markdown": opts.IncludeMarkdown,
		"save_artifacts":   opts.SaveArtifacts,
	})(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return nil, err
	}
	// markdown requires html
	includeHTML := opts.IncludeHTML || opts.SaveArtifacts
	includeMD := opts.IncludeMarkdown || opts.SaveArtifacts
	snap, err = perception.Capture(b.Ctx(), includeHTML, includeMD)
	if err != nil {
		return nil, wrap(err)
	}
	e.mu.Lock()
	e.lastRefMap = snap.RefMap
	e.mu.Unlock()
	if opts.SaveArtifacts && e.session != nil {
		_ = e.session.SavePage(snap.URL, []byte(snap.HTML), []byte(snap.Markdown), nil)
	}
	if !opts.IncludeHTML {
		snap.HTML = ""
	}
	if !opts.IncludeMarkdown {
		snap.Markdown = ""
	}
	return snap, nil
}

// --- click / type ---------------------------------------------------------

func (e *Engine) backendIDForRef(ref string) (cdp.BackendNodeID, error) {
	e.mu.Lock()
	id, ok := e.lastRefMap[ref]
	e.mu.Unlock()
	if !ok {
		return 0, newErr(CodeNotFound, "unknown ref %q (call snapshot first)", ref)
	}
	return cdp.BackendNodeID(id), nil
}

func (e *Engine) Click(ref string) (err error) {
	defer e.record("click", map[string]any{"ref": ref})(&err)
	bid, err := e.backendIDForRef(ref)
	if err != nil {
		return err
	}
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	err = chromedp.Run(b.Ctx(), chromedp.ActionFunc(func(ctx context.Context) error {
		if err := cdpdom.ScrollIntoViewIfNeeded().WithBackendNodeID(bid).Do(ctx); err != nil {
			return err
		}
		box, err := cdpdom.GetBoxModel().WithBackendNodeID(bid).Do(ctx)
		if err != nil {
			return err
		}
		cx, cy := quadCenter(box.Content)
		if err := (input.DispatchMouseEvent(input.MousePressed, cx, cy).
			WithButton(input.Left).WithClickCount(1)).Do(ctx); err != nil {
			return err
		}
		return (input.DispatchMouseEvent(input.MouseReleased, cx, cy).
			WithButton(input.Left).WithClickCount(1)).Do(ctx)
	}))
	return wrapNil(err)
}

func (e *Engine) Type(ref, text string, pressEnter bool) (err error) {
	defer e.record("type", map[string]any{"ref": ref, "text": text, "press_enter": pressEnter})(&err)
	bid, err := e.backendIDForRef(ref)
	if err != nil {
		return err
	}
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	err = chromedp.Run(b.Ctx(), chromedp.ActionFunc(func(ctx context.Context) error {
		if err := cdpdom.Focus().WithBackendNodeID(bid).Do(ctx); err != nil {
			return err
		}
		if text != "" {
			if err := input.InsertText(text).Do(ctx); err != nil {
				return err
			}
		}
		if pressEnter {
			return chromedp.KeyEvent("\r").Do(ctx)
		}
		return nil
	}))
	return wrapNil(err)
}

// --- screenshot -----------------------------------------------------------

func (e *Engine) Screenshot(fullPage bool) (buf []byte, err error) {
	defer e.record("screenshot", map[string]any{"full_page": fullPage})(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return nil, err
	}
	var action chromedp.Action
	if fullPage {
		action = chromedp.FullScreenshot(&buf, 90)
	} else {
		action = chromedp.CaptureScreenshot(&buf)
	}
	if rerr := chromedp.Run(b.Ctx(), action); rerr != nil {
		return nil, wrap(rerr)
	}
	return buf, nil
}

// --- eval ----------------------------------------------------------------

func (e *Engine) Eval(js string) (raw json.RawMessage, err error) {
	defer e.record("eval", map[string]any{"js": js})(&err)
	b, err := e.EnsureBrowser()
	if err != nil {
		return nil, err
	}
	err = chromedp.Run(b.Ctx(), chromedp.ActionFunc(func(ctx context.Context) error {
		res, exc, err := runtime.Evaluate(js).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			// exc.Error() includes the exception text, line/col, and the
			// remote object's Description (which carries the stack trace
			// for thrown Errors). Far more useful than just exc.Text.
			return newErr(CodeEvalThrew, "%s", exc.Error())
		}
		if res != nil {
			raw = json.RawMessage(res.Value)
		}
		return nil
	}))
	if err != nil {
		return nil, wrap(err)
	}
	return raw, nil
}

// --- wait_for ------------------------------------------------------------

// WaitFor supports:
//
//	selector:<css>   wait until selector matches (default if no prefix)
//	js:<expr>        wait until expression is truthy
//	url:<substr>     wait until current URL contains substr
//	load             wait for the page load lifecycle event
func (e *Engine) WaitFor(condition string, timeout time.Duration) (err error) {
	defer e.record("wait_for", map[string]any{"condition": condition, "timeout_ms": timeout.Milliseconds()})(&err)
	if timeout <= 0 {
		// 15s default. The Gmail compose-send agent run (PRD §6 Pattern C)
		// burned ~20 polling evals after a 10s wait_for missed Gmail's
		// modal-open. 15s catches slow modals without making timeout
		// failures feel sluggish.
		timeout = 15 * time.Second
	}
	b, err := e.EnsureBrowser()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(b.Ctx(), timeout)
	defer cancel()
	werr := e.waitForInner(ctx, condition)
	if werr == nil {
		return nil
	}
	if errors.Is(werr, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return &Error{Code: CodeTimeout, Message: fmt.Sprintf("wait_for(%q) timed out after %s", condition, timeout), Cause: werr}
	}
	return wrap(werr)
}

func (e *Engine) waitForInner(ctx context.Context, condition string) error {
	if condition == "load" {
		return pollUntil(ctx, func() (bool, error) {
			raw, err := e.Eval("document.readyState === 'complete'")
			if err != nil {
				return false, err
			}
			var b bool
			_ = json.Unmarshal(raw, &b)
			return b, nil
		})
	}

	prefix, arg := splitCondition(condition)
	switch prefix {
	case "url":
		return pollUntil(ctx, func() (bool, error) {
			var u string
			if err := chromedp.Run(ctx, chromedp.Location(&u)); err != nil {
				return false, err
			}
			return strings.Contains(u, arg), nil
		})
	case "js":
		expr := fmt.Sprintf("Boolean(%s)", arg)
		return pollUntil(ctx, func() (bool, error) {
			raw, err := e.Eval(expr)
			if err != nil {
				return false, err
			}
			var b bool
			_ = json.Unmarshal(raw, &b)
			return b, nil
		})
	default: // selector
		return chromedp.Run(ctx, chromedp.WaitVisible(arg, chromedp.ByQuery))
	}
}

func splitCondition(c string) (prefix, arg string) {
	for _, p := range []string{"selector:", "js:", "url:"} {
		if strings.HasPrefix(c, p) {
			return strings.TrimSuffix(p, ":"), strings.TrimPrefix(c, p)
		}
	}
	return "selector", c
}

func pollUntil(ctx context.Context, check func() (bool, error)) error {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			ok, err := check()
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
	}
}

// quadCenter returns the centroid of a 4-point quad (8 floats, x,y pairs).
func quadCenter(quad []float64) (float64, float64) {
	if len(quad) < 8 {
		return 0, 0
	}
	var sx, sy float64
	for i := 0; i < 8; i += 2 {
		sx += quad[i]
		sy += quad[i+1]
	}
	return sx / 4, sy / 4
}
