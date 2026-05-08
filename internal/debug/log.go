// Package debug is a tail-friendly file logger for live-debugging an MCP run.
//
// Activated by setting LLMLENS_DEBUG_LOG to a writable file path before
// launching the MCP server. When the env var is unset, every Logf call is
// a near-zero-cost no-op (atomic check + early return).
//
// Format is plain text, one event per line:
//
//	<RFC3339Nano timestamp> [<component>] <message>
//
// Tail with `tail -f $LLMLENS_DEBUG_LOG`. The log is intentionally not
// rotated — short MCP sessions don't need it, and `truncate -s 0 file`
// works fine for clearing between runs.
//
// Why an env var rather than a flag: Claude Code spawns the MCP server as
// a child process and inherits the parent shell's environment. Setting
// LLMLENS_DEBUG_LOG in your shell, then launching/restarting Claude Code,
// applies without editing ~/.claude.json.
package debug

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const envVar = "LLMLENS_DEBUG_LOG"

var (
	mu     sync.Mutex
	out    *os.File
	active bool
)

// Init opens the debug log file specified by LLMLENS_DEBUG_LOG. Safe to
// call multiple times; subsequent calls are no-ops. If the env var is
// unset or the file cannot be opened, debug logging stays inactive.
//
// Errors are written to stderr but never fatal — debug logging being
// unavailable should not break the agent.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	if active {
		return
	}
	path := os.Getenv(envVar)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmlens debug log: cannot open %s: %v\n", path, err)
		return
	}
	out = f
	active = true
	// Banner so tail-f users see when a fresh session starts.
	_, _ = fmt.Fprintf(out, "%s [debug] log opened (pid=%d)\n",
		time.Now().UTC().Format(time.RFC3339Nano), os.Getpid())
}

// Logf writes a timestamped line to the debug log, scoped to a component
// (e.g. "engine", "registry", "tool"). No-op if Init wasn't called or
// the env var was unset.
//
// Component is the high-level subsystem; format / args is a normal
// Printf-style message. Don't put newlines in the message — one event
// per line keeps grepping cheap.
func Logf(component, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !active {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(out, "%s [%s] %s\n", ts, component, msg)
}

// Close flushes and closes the debug log. Safe to call when not active.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if !active {
		return
	}
	_, _ = fmt.Fprintf(out, "%s [debug] log closed\n",
		time.Now().UTC().Format(time.RFC3339Nano))
	_ = out.Close()
	out = nil
	active = false
}

// Active reports whether debug logging is on. Useful for skipping
// expensive context-collection work when nobody's watching.
func Active() bool {
	mu.Lock()
	defer mu.Unlock()
	return active
}
