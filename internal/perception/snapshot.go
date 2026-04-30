package perception

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	cdpdom "github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"llmlens/internal/auth"
)

type Element struct {
	Ref           string `json:"ref"`
	Role          string `json:"role"`
	Name          string `json:"name,omitempty"`
	Value         string `json:"value,omitempty"`
	Description   string `json:"description,omitempty"`
	Href          string `json:"href,omitempty"`  // populated for role=link
	Frame         string `json:"frame,omitempty"` // empty = main frame; otherwise sub-frame ID
	Focusable     bool   `json:"focusable,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	BackendNodeID int64  `json:"-"`
}

type Snapshot struct {
	URL          string           `json:"url"`
	Title        string           `json:"title"`
	CapturedAt   time.Time        `json:"captured_at"`
	Elements     []Element        `json:"elements"`
	Markdown     string           `json:"markdown,omitempty"`
	HTML         string           `json:"html,omitempty"`
	AuthRequired bool             `json:"auth_required,omitempty"` // page looks like a login wall
	AuthHint     string           `json:"auth_hint,omitempty"`     // human-readable next step for the agent
	RefMap       map[string]int64 `json:"-"`
}

// roles we surface in the structured element list. anything else is left to markdown.
var surfacedRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"combobox": true, "checkbox": true, "radio": true, "menuitem": true,
	"tab": true, "switch": true, "slider": true, "spinbutton": true,
	"option": true, "heading": true, "image": true,
}

// Capture pulls the current AXTree (across every frame) and the main-frame
// outerHTML, fuses them into a Snapshot, and renders markdown.
// includeHTML/includeMarkdown let callers trade tokens for fidelity.
func Capture(ctx context.Context, includeHTML, includeMarkdown bool) (*Snapshot, error) {
	var url, title, html string
	var axNodes []frameNode

	if err := chromedp.Run(ctx,
		chromedp.Location(&url),
		chromedp.Title(&title),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			axNodes, err = collectAXTreesAllFrames(ctx)
			return err
		}),
	); err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}

	if includeHTML || includeMarkdown {
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			root, err := cdpdom.GetDocument().Do(ctx)
			if err != nil {
				return err
			}
			html, err = cdpdom.GetOuterHTML().WithNodeID(root.NodeID).Do(ctx)
			return err
		})); err != nil {
			return nil, fmt.Errorf("get outer html: %w", err)
		}
	}

	snap := &Snapshot{
		URL:        url,
		Title:      title,
		CapturedAt: time.Now().UTC(),
		RefMap:     map[string]int64{},
	}

	refCounter := 0
	for _, fn := range axNodes {
		n := fn.node
		if n.Ignored || n.BackendDOMNodeID == 0 {
			continue
		}
		role := axString(n.Role)
		focusable := axBool(propertyValue(n, "focusable"))
		if !surfacedRoles[role] && !focusable {
			continue
		}
		refCounter++
		ref := fmt.Sprintf("e%d", refCounter)
		backendID := int64(n.BackendDOMNodeID)
		snap.Elements = append(snap.Elements, Element{
			Ref:           ref,
			Role:          role,
			Name:          strings.TrimSpace(axString(n.Name)),
			Value:         axString(n.Value),
			Description:   axString(n.Description),
			Frame:         fn.frameID,
			Focusable:     focusable,
			Disabled:      axBool(propertyValue(n, "disabled")),
			BackendNodeID: backendID,
		})
		snap.RefMap[ref] = backendID
	}

	// Hydrate hrefs for link-roled elements. Without this, agents need eval
	// to extract any URL — defeats the point of snapshot for navigation tasks.
	// One DescribeNode call per link; cheap relative to the AXTree walk.
	if err := hydrateLinkHrefs(ctx, snap.Elements); err != nil {
		// Non-fatal: link hrefs are nice-to-have. Snapshot still useful.
		_ = err
	}

	detectAuthWall(snap)

	if includeHTML {
		snap.HTML = html
	}
	if includeMarkdown {
		md, err := HTMLToMarkdown(html)
		if err == nil {
			snap.Markdown = md
		}
	}

	return snap, nil
}

// frameNode pairs an accessibility node with the frame it came from. We
// flatten across all frames into one element list, but stash the frame id
// so it can surface on Element for callers who care.
type frameNode struct {
	node    *accessibility.Node
	frameID string // empty == main frame
}

// collectAXTreesAllFrames walks the page's frame tree and pulls the AXTree
// for each frame. Cross-origin frames whose AXTree is unavailable are
// silently skipped — they'd return a CDP error and contribute nothing
// useful. Same-origin sub-frames (Gmail message-body sandbox, embedded
// content) get fully traversed.
func collectAXTreesAllFrames(ctx context.Context) ([]frameNode, error) {
	tree, err := page.GetFrameTree().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("get frame tree: %w", err)
	}

	var frameIDs []cdp.FrameID
	var walk func(*page.FrameTree)
	walk = func(ft *page.FrameTree) {
		if ft == nil || ft.Frame == nil {
			return
		}
		frameIDs = append(frameIDs, ft.Frame.ID)
		for _, child := range ft.ChildFrames {
			walk(child)
		}
	}
	walk(tree)

	var out []frameNode
	for i, fid := range frameIDs {
		var nodes []*accessibility.Node
		var err error
		if i == 0 {
			// Root frame: passing FrameID is redundant.
			nodes, err = accessibility.GetFullAXTree().Do(ctx)
		} else {
			nodes, err = accessibility.GetFullAXTree().WithFrameID(fid).Do(ctx)
		}
		if err != nil {
			// Cross-origin frames refuse — keep going on the others.
			continue
		}
		fidStr := ""
		if i > 0 {
			fidStr = string(fid)
		}
		for _, n := range nodes {
			out = append(out, frameNode{node: n, frameID: fidStr})
		}
	}
	return out, nil
}

// detectAuthWall flags the snapshot when the page looks like a login flow,
// so an agent can stop and ask the user to run `llmlens auth-start --domain=...`
// instead of trying to continue past it.
//
// Heuristic: URL matches a login pattern, OR the page contains a password
// textbox + a sign-in button. Either is a strong signal; both together are
// near-certain. False positives just nudge the agent unnecessarily; false
// negatives let it spin trying to interact with auth-gated content.
func detectAuthWall(s *Snapshot) {
	if auth.IsLoginURL(s.URL) {
		s.AuthRequired = true
		s.AuthHint = authHint(s.URL)
		return
	}
	var hasPasswordField, hasSignInButton bool
	for _, el := range s.Elements {
		switch el.Role {
		case "textbox":
			n := strings.ToLower(el.Name + " " + el.Description)
			if strings.Contains(n, "password") || strings.Contains(n, "email") || strings.Contains(n, "username") {
				hasPasswordField = true
			}
		case "button":
			n := strings.ToLower(el.Name)
			if strings.Contains(n, "sign in") || strings.Contains(n, "signin") ||
				strings.Contains(n, "log in") || strings.Contains(n, "login") ||
				strings.Contains(n, "continue") || strings.Contains(n, "next") {
				hasSignInButton = true
			}
		}
	}
	if hasPasswordField && hasSignInButton {
		s.AuthRequired = true
		s.AuthHint = authHint(s.URL)
	}
}

// authHint builds a short, action-oriented note for the agent to relay.
func authHint(rawURL string) string {
	host := hostOf(rawURL)
	if host == "" {
		return "This page looks like a login wall. Ask the user to run `llmlens auth-start --domain=<the target domain>` and re-register the MCP server with the new profile."
	}
	return "This page looks like a login wall (" + host + "). Ask the user to run `llmlens auth-start --domain=" + host + "` and re-register the MCP server with the new profile."
}

func hostOf(rawURL string) string {
	// strip scheme
	s := rawURL
	for _, p := range []string{"https://", "http://"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			s = s[len(p):]
			break
		}
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// hydrateLinkHrefs populates Element.Href for every Element with role=="link"
// by issuing a DescribeNode call against its backend node id and reading the
// "href" attribute. Mutates elements in place. Errors are returned but the
// caller may treat them as non-fatal — a snapshot without hrefs is still
// useful, just less powerful for navigation-shaped agent tasks.
func hydrateLinkHrefs(ctx context.Context, elements []Element) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for i := range elements {
			if elements[i].Role != "link" || elements[i].BackendNodeID == 0 {
				continue
			}
			node, err := cdpdom.DescribeNode().
				WithBackendNodeID(cdp.BackendNodeID(elements[i].BackendNodeID)).
				Do(ctx)
			if err != nil {
				continue // skip this element, keep going
			}
			if href, ok := node.Attribute("href"); ok {
				elements[i].Href = href
			}
		}
		return nil
	}))
}

func axString(v *accessibility.Value) string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err == nil {
		return s
	}
	return string(v.Value)
}

func axBool(v *accessibility.Value) bool {
	if v == nil || len(v.Value) == 0 {
		return false
	}
	var b bool
	_ = json.Unmarshal(v.Value, &b)
	return b
}

func propertyValue(n *accessibility.Node, name string) *accessibility.Value {
	for _, p := range n.Properties {
		if string(p.Name) == name {
			return p.Value
		}
	}
	return nil
}
