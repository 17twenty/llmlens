package rpc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"llmlens/internal/tools"
)

// Mode selects the JSON-RPC envelope shape.
//
//	ModeJSONRPC — line-delimited bare JSON-RPC 2.0. Methods are tool names
//	              ("navigate", "snapshot", ...). Useful for scripting and tests.
//	ModeMCP     — Model Context Protocol over stdio. Methods are
//	              "initialize", "tools/list", "tools/call". Use this for
//	              integration with Claude Code or other MCP clients.
//
// Both modes share the same engine and tool dispatch internally.
type Mode int

const (
	ModeJSONRPC Mode = iota
	ModeMCP
)

type Server struct {
	engine *tools.Engine
	in     io.Reader
	out    io.Writer
	mode   Mode
}

func New(e *tools.Engine, in io.Reader, out io.Writer) *Server {
	return &Server{engine: e, in: in, out: out, mode: ModeJSONRPC}
}

func (s *Server) WithMode(m Mode) *Server {
	s.mode = m
	return s
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// isNotification returns true for JSON-RPC notifications (no id field).
// Per spec we never send a response for notifications.
func (r *request) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(s.out)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(response{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		var resp response
		var skip bool
		switch s.mode {
		case ModeMCP:
			resp, skip = s.dispatchMCP(req)
		default:
			resp = s.dispatchJSONRPC(req)
		}
		if skip {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) dispatchJSONRPC(req request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if req.Method == "tools.list" {
		resp.Result = jsonrpcToolsList()
		return resp
	}
	result, err := s.handle(req.Method, req.Params)
	if err != nil {
		resp.Error = toRPCErr(err)
		return resp
	}
	resp.Result = result
	return resp
}

// toRPCErr maps a tools.Error category onto JSON-RPC error.data so agents can
// branch on category without parsing the message.
func toRPCErr(err error) *rpcErr {
	out := &rpcErr{Code: -32000, Message: err.Error()}
	var te *tools.Error
	if errors.As(err, &te) {
		out.Data = map[string]any{"category": string(te.Code)}
	}
	return out
}

// handle runs a tool by its bare name. Shared between the JSON-RPC and MCP
// transports — only the request/response envelope differs.
func (s *Server) handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "navigate":
		var p struct {
			URL string `json:"url"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		if err := s.engine.Navigate(p.URL); err != nil {
			return nil, err
		}
		return map[string]string{"url": p.URL}, nil

	case "back":
		return map[string]bool{"ok": true}, s.engine.Back()
	case "forward":
		return map[string]bool{"ok": true}, s.engine.Forward()
	case "reload":
		return map[string]bool{"ok": true}, s.engine.Reload()

	case "snapshot":
		var p tools.SnapshotOpts
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		snap, err := s.engine.Snapshot(p)
		if err != nil {
			return nil, err
		}
		return snap, nil

	case "click":
		var p struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		return map[string]string{"ref": p.Ref}, s.engine.Click(p.Ref)

	case "type":
		var p struct {
			Ref        string `json:"ref"`
			Text       string `json:"text"`
			PressEnter bool   `json:"press_enter,omitempty"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		return map[string]any{"ref": p.Ref}, s.engine.Type(p.Ref, p.Text, p.PressEnter)

	case "screenshot":
		var p struct {
			FullPage bool `json:"full_page,omitempty"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		png, err := s.engine.Screenshot(p.FullPage)
		if err != nil {
			return nil, err
		}
		return map[string]string{"png_base64": base64.StdEncoding.EncodeToString(png)}, nil

	case "eval":
		var p struct {
			JS string `json:"js"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		raw, err := s.engine.Eval(p.JS)
		if err != nil {
			return nil, err
		}
		return map[string]json.RawMessage{"result": raw}, nil

	case "wait_for":
		var p struct {
			Condition string `json:"condition"`
			TimeoutMS int    `json:"timeout_ms,omitempty"`
		}
		if err := unmarshalParams(params, &p); err != nil {
			return nil, err
		}
		dur := time.Duration(p.TimeoutMS) * time.Millisecond
		return map[string]string{"condition": p.Condition}, s.engine.WaitFor(p.Condition, dur)

	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

func unmarshalParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// jsonrpcToolsList is a terse self-description for the bare JSON-RPC mode.
// MCP mode uses richer toolDescriptors() with full JSON Schema (see mcp.go).
func jsonrpcToolsList() []map[string]any {
	out := make([]map[string]any, 0, len(toolDescriptors()))
	for _, t := range toolDescriptors() {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
		})
	}
	return out
}
