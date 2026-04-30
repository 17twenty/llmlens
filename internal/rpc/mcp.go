package rpc

import (
	"encoding/json"
	"fmt"
)

// MCP is implemented over the same line-delimited JSON-RPC 2.0 transport, but
// dispatches three named methods (initialize / tools/list / tools/call) and
// adapts our internal tool results into the MCP "content array" response shape.
//
// Spec reference: https://spec.modelcontextprotocol.io/specification/2024-11-05/

const mcpProtocolVersion = "2024-11-05"
const mcpServerName = "llmlens"
const mcpServerVersion = "0.1.0"

// dispatchMCP returns the response and a "skip send" flag for notifications.
func (s *Server) dispatchMCP(req request) (response, bool) {
	resp := response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    mcpServerName,
				"version": mcpServerVersion,
			},
		}
		return resp, false

	case "notifications/initialized":
		// notification, no response
		return resp, true

	case "ping":
		resp.Result = map[string]any{}
		return resp, false

	case "tools/list":
		resp.Result = map[string]any{
			"tools": toolDescriptors(),
		}
		return resp, false

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp, false
		}
		result, err := s.handle(p.Name, p.Arguments)
		resp.Result = mcpToolCallResult(p.Name, result, err)
		return resp, false

	default:
		// notifications we don't care about (e.g. notifications/cancelled)
		// should be silently ignored.
		if req.isNotification() {
			return resp, true
		}
		resp.Error = &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
		return resp, false
	}
}

// mcpToolCallResult formats one tool invocation as MCP content. Errors are
// returned as a content array with isError=true rather than via the protocol
// error channel — this is the spec's preferred shape because it lets the
// model see and react to the failure text.
func mcpToolCallResult(name string, result any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("%s failed: %s", name, err.Error()),
			}},
			"isError": true,
		}
	}

	// Special-case: screenshot returns base64 PNG; surface as MCP image content.
	if name == "screenshot" {
		if m, ok := result.(map[string]string); ok {
			if b64, ok := m["png_base64"]; ok {
				return map[string]any{
					"content": []map[string]any{{
						"type":     "image",
						"data":     b64,
						"mimeType": "image/png",
					}},
				}
			}
		}
	}

	// Default: marshal the result as JSON text. Agents parse this fine and
	// it preserves all structured data without needing per-tool adapters.
	body, mErr := json.Marshal(result)
	if mErr != nil {
		body = []byte(fmt.Sprintf("%v", result))
	}
	return map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(body),
		}},
	}
}

// toolDescriptor is the MCP shape for one tool. inputSchema is JSON Schema.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolDescriptors is the source of truth for tool metadata. JSON-RPC mode's
// terser tools.list is generated from these; MCP returns them verbatim.
//
// Descriptions are written for an LLM consumer — keep them tight, name the
// expected effect, document non-obvious params.
func toolDescriptors() []toolDescriptor {
	objSchema := func(props map[string]any, required ...string) map[string]any {
		if props == nil {
			props = map[string]any{}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	boolean := func(desc string) map[string]any {
		return map[string]any{"type": "boolean", "description": desc}
	}
	integer := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}

	return []toolDescriptor{
		{
			Name:        "navigate",
			Description: "Navigate to a URL. Returns the requested URL on success.",
			InputSchema: objSchema(map[string]any{
				"url": str("Destination URL, including scheme."),
			}, "url"),
		},
		{
			Name:        "back",
			Description: "Browser history: go back one entry.",
			InputSchema: objSchema(nil),
		},
		{
			Name:        "forward",
			Description: "Browser history: go forward one entry.",
			InputSchema: objSchema(nil),
		},
		{
			Name:        "reload",
			Description: "Reload the current page.",
			InputSchema: objSchema(nil),
		},
		{
			Name: "snapshot",
			Description: "Capture a structured perception of the current page from the AXTree, " +
				"traversing every same-origin sub-frame as well — so message bodies, embedded " +
				"widgets, and other iframe content are included alongside the main frame. " +
				"Returns elements (with refs usable by click/type), URL, title. Each element has " +
				"role, name, value, description; elements with role=link also include href so you " +
				"can collect URLs without resorting to eval. Sub-frame elements include a frame " +
				"field identifying their origin. " +
				"include_markdown adds a markdown render (token-heavy). save_artifacts persists " +
				"html+md to runs/<id>/. " +
				"Call snapshot before each click/type — refs are only valid against the latest snapshot. " +
				"Prefer snapshot over eval for structured-data tasks: it's cheaper, safer, and the " +
				"AXTree often already exposes what you'd otherwise scrape via querySelectorAll. " +
				"If auth_required is true on the response, the page is a login wall — stop the task " +
				"and report auth_hint to the user; do not attempt to bypass. " +
				"If vision_recommended is true, the page contains content the AXTree cannot see " +
				"(canvas-rendered apps like Google Maps / Sheets / Figma, charts, dense interactive " +
				"grids). Call screenshot() and use vision to interpret what the structured " +
				"elements miss. The vision_reason field tells you why the hint fired.",
			InputSchema: objSchema(map[string]any{
				"include_html":     boolean("Include raw HTML in the response. Very token-heavy; usually leave false."),
				"include_markdown": boolean("Include a markdown render of the page. Useful for content extraction."),
				"save_artifacts":   boolean("Persist html + markdown under runs/<id>/<host>/<path>. Recommended."),
			}),
		},
		{
			Name:        "click",
			Description: "Click an element by ref from the latest snapshot, e.g. \"e7\". Mouse-event based; scrolls into view first.",
			InputSchema: objSchema(map[string]any{
				"ref": str("Element ref like \"e7\" from the latest snapshot."),
			}, "ref"),
		},
		{
			Name:        "type",
			Description: "Focus an element by ref and type text. Set press_enter=true to submit forms.",
			InputSchema: objSchema(map[string]any{
				"ref":         str("Element ref like \"e7\" from the latest snapshot."),
				"text":        str("Text to insert. May be empty to only press Enter."),
				"press_enter": boolean("Press Enter after typing. Useful for submitting search boxes."),
			}, "ref", "text"),
		},
		{
			Name: "screenshot",
			Description: "Capture the current viewport (or full page) as PNG. Returned as MCP image content. " +
				"Use this when the AXTree-derived snapshot is missing information (canvas-rendered apps, " +
				"poorly-marked-up pages, visual layout questions).",
			InputSchema: objSchema(map[string]any{
				"full_page": boolean("If true, capture the full scrollable page; otherwise just the viewport."),
			}),
		},
		{
			Name: "eval",
			Description: "Execute JavaScript in the page context. Returns the result as JSON. " +
				"Escape hatch — use sparingly; prefer click/type/snapshot when possible.",
			InputSchema: objSchema(map[string]any{
				"js": str("JavaScript expression. Last expression's value is returned."),
			}, "js"),
		},
		{
			Name: "wait_for",
			Description: "Wait for a page condition. Conditions: \"load\" (document.readyState=='complete'), " +
				"\"url:<substring>\" (current URL contains substring), \"js:<expr>\" (JS expression is truthy), " +
				"or a CSS selector (default — waits for first match to be visible).",
			InputSchema: objSchema(map[string]any{
				"condition":  str("Condition string. See description for prefixes."),
				"timeout_ms": integer("Timeout in milliseconds. Default 10000."),
			}, "condition"),
		},
	}
}
