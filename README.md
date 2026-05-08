# LLMLens

Thin, CDP-native browser harness for LLM agents. Skips Playwright/Puppeteer and
binds to Chrome DevTools Protocol directly via a small Go layer so the tool
surface stays minimal, the token footprint stays low, and the agent stays in
control.

Designed around four ideas:

1. **AXTree-first perception.** Snapshots come from the accessibility tree by
   default; HTML/markdown/screenshots are opt-in escalations.
2. **Sharp tool surface.** Eleven tools, no framework cruft. An agent can hold
   the whole API in working memory.
3. **Attached-session auth.** Log in once by hand, capture credentials to a
   portable bundle, hand to the agent.
4. **Two transports, one engine.** Same dispatch behind both line-delimited
   JSON-RPC (scripting / tests) and the Model Context Protocol (Claude Code
   and other MCP-aware clients).

Status: pre-1.0. Smokes pass against Google search, LinkedIn (search +
feed), and Gmail (inbox + iframe-rendered message bodies). Agent-driven
scenarios validated end-to-end against LinkedIn and Gmail via MCP.

Known caveat: Google session cookies rotate aggressively — a Gmail bundle
captured via `auth-start` can go stale within ~30 minutes, requiring a
re-run. LinkedIn bundles are long-lived. See PRD §3.3 for the trigger
list that would justify the Chrome-extension transport over the current
bundle approach.

See `PRD.md` for design intent and `smoketests.md` for what's validated.

## Install

You also need Google Chrome installed at the default OS path (or pass
`--chrome=<path>` to override). llmlens drives a Chrome instance via
CDP — it doesn't ship its own browser.

### Pre-built binaries (recommended)

Each release archive contains `llmlens` + `smoketest` plus the
project documentation. Grab the right one for your platform from the
[latest release](https://github.com/17twenty/llmlens/releases/latest):

| Target | Archive |
|--------|---------|
| macOS Apple Silicon | `llmlens_<version>_darwin_arm64.tar.gz` |
| macOS Intel | `llmlens_<version>_darwin_amd64.tar.gz` |
| Linux x86_64 | `llmlens_<version>_linux_amd64.tar.gz` |
| Linux ARM64 | `llmlens_<version>_linux_arm64.tar.gz` |
| Windows x86_64 | `llmlens_<version>_windows_amd64.zip` |

Verify against `checksums.txt` in the same release, then extract
anywhere on `$PATH`. Both binaries are self-contained.

### From source

Requires Go 1.26+.

```bash
git clone https://github.com/17twenty/llmlens
cd llmlens
go build -o bin/llmlens   ./cmd/llmlens
go build -o bin/smoketest ./cmd/smoketest
```

Verify the build with the no-auth smokes:

```bash
./bin/smoketest -scenario=snapshot-shape -headless=true
./bin/smoketest -scenario=google-discovery
```

Both should end in `PASS`.

## Getting started

Three steps: capture credentials for a site, verify the capture, register
llmlens as an MCP server in Claude Code.

### 1. Capture credentials

`auth-start` opens a dedicated headed Chrome on a temp profile. Log in by
hand. Capture fires automatically once the URL stabilises on a non-login
page (default 3s settle).

```bash
mkdir -p profiles
./bin/llmlens auth-start -domain=linkedin.com -out=profiles/linkedin.json
```

Watch stderr for the URL trace — useful for confirming the login flow
finished before capture. The bundle is JSON: cookies + per-origin web
storage. The temp profile is naturally scoped to login-flow cookies, so
federated auth (e.g. `accounts.google.com` ↔ `mail.google.com`) works
without extra configuration.

### 2. Verify the bundle is authenticated

```bash
./bin/smoketest -scenario=creds-roundtrip -profile=profiles/linkedin.json
```

PASS output ends with `authenticated /feed/ loaded with N/5 auth
markers`. Failure modes (`AUTH FAILURE` / `VERIFICATION CHALLENGE` /
`AMBIGUOUS`) are interpreted in `smoketests.md`.

For Gmail specifically, also run `gmail-triage`:

```bash
./bin/smoketest -scenario=gmail-triage -profile=profiles/gmail.json
```

### 3. Register the MCP server

One MCP entry covers every credential bundle in `profiles/`. Add this to
`~/.claude.json` (user scope) or `./.claude/settings.json` (project
scope, recommended):

```json
{
  "mcpServers": {
    "llmlens": {
      "command": "/absolute/path/to/llmlens/bin/llmlens",
      "args": [
        "serve",
        "--protocol=mcp",
        "--profiles-dir=/absolute/path/to/llmlens/profiles"
      ]
    }
  }
}
```

Use absolute paths — Claude Code launches the server from its own
working directory.

**No re-registration when you add a new domain.** Every `*.json` in
`profiles/` loads on browser launch, and `fsnotify` watches the
directory for changes — drop a new bundle in and the running MCP
server hot-imports it within seconds.

```bash
./bin/llmlens auth-start -domain=mail.google.com -out=profiles/gmail.json
# That's it — your active Claude conversation now has Gmail auth too.
```

Restart Claude Code after the *first* registration so the MCP server
launches. After that, only adding a *new* MCP server (e.g. a different
project's llmlens) requires a restart.

### Example agent prompt

```
Use the llmlens tools to find my 1st and 2nd order LinkedIn connections
who work in AI/ML/LLMs. For each: name, headline, company, profile URL.
Save to `runs/<id>/connections.csv`. Pace requests to avoid rate limits.
```

More scenarios with run prompts and pass criteria are in `smoketests.md`.

### CLI registration (if you have `claude` on `$PATH`)

The `claude` binary is only present if you installed Claude Code via
`npm i -g @anthropic-ai/claude-code` or the official installer. If
you're hosted via Zed (or any other editor that bundles Claude Code as a
sidecar), you won't have it — use the JSON edit above.

```bash
claude mcp add llmlens "$PWD/bin/llmlens" \
  -- serve --protocol=mcp --profiles-dir="$PWD/profiles"
```

## Tools

All tools available in both JSON-RPC and MCP modes.

| Name        | Purpose                                                     |
|-------------|-------------------------------------------------------------|
| `navigate`  | Go to a URL.                                                |
| `back`      | Browser history: back.                                      |
| `forward`   | Browser history: forward.                                   |
| `reload`    | Reload current page.                                        |
| `close_browser` | Tear down Chrome; next tool call lazy-relaunches a clean instance with cookies + listeners re-imported. Use when finished or when something feels stuck. |
| `snapshot`  | Capture AXTree-derived element list across every frame (link elements include `href`, sub-frame elements include `frame`); flags `auth_required` for login walls and `vision_recommended` when the page is canvas-rendered or AXTree-starved; optional HTML/markdown.|
| `click`     | Click element by ref from the latest snapshot.              |
| `type`      | Focus an element, type text, optional Enter to submit.      |
| `screenshot`| PNG of viewport or full page (returns base64 / MCP image).  |
| `eval`      | Run JS in page; result returned as JSON.                    |
| `wait_for`  | Wait for `load` / `url:<substr>` / `js:<expr>` / CSS sel.   |

Refs are produced by `snapshot` (e.g. `e7`) and are valid only against the
latest snapshot. Re-snapshot before further interaction.

Tool calls are bounded by per-call timeouts: 30s for navigation-shaped
operations (`navigate`, `back`, `forward`, `reload`), 15s for the rest.
A genuine hang surfaces as a typed `timeout` error within seconds rather
than wedging the entire MCP server. `wait_for` retains its own
caller-supplied `timeout_ms` (default 15s). The engine self-heals from
dead browser contexts: if Chrome dies, the next tool call drops the
stale handle and lazy-relaunches transparently.

## JSON-RPC mode (for scripting and tests)

Default if `--protocol` is omitted. Line-delimited JSON-RPC 2.0 on stdio.

```bash
./bin/llmlens serve -mode=launch -profile=profiles/linkedin.json
```

Then on stdin, one JSON object per line:

```json
{"jsonrpc":"2.0","id":1,"method":"navigate","params":{"url":"https://www.linkedin.com/feed/"}}
{"jsonrpc":"2.0","id":2,"method":"wait_for","params":{"condition":"load","timeout_ms":15000}}
{"jsonrpc":"2.0","id":3,"method":"snapshot","params":{"include_markdown":true,"save_artifacts":true}}
```

Errors carry a stable category in `error.data.category` so callers can
branch without parsing strings:

```json
{"jsonrpc":"2.0","id":4,"error":{"code":-32000,"message":"...","data":{"category":"stale"}}}
```

Categories: `not_found`, `stale`, `timeout`, `navigation_failed`,
`eval_threw`, `invalid_param`, `internal`.

## Modes

- **Launch (default).** Spawns a Chrome instance scoped to llmlens. Use
  `--profiles-dir=<dir>` to load every credential bundle in that dir on
  launch, plus hot-reload new ones added at runtime.
- **Attach.** Connects to a Chrome already running with
  `--remote-debugging-port=9222`. Useful for advanced workflows; most
  users want the launch mode.

```bash
./bin/llmlens serve -mode=attach -remote=http://localhost:9222
```

## Session artifacts

Every run produces a directory under `runs/<run-id>/`:

```
runs/20260430T060958Z-4735bdda/
  events.log                         # JSONL audit of every tool call
  www.linkedin.com/
    feed.md                          # markdown render
    feed.html                        # raw HTML
    search_q_q-ai.md
    ...
```

`events.log` is one JSON object per line:

```json
{"ts":"2026-04-30T...","tool":"snapshot","params":{...},"duration_ms":23,"ok":true}
{"ts":"2026-04-30T...","tool":"click","params":{"ref":"e7"},"duration_ms":18,"ok":false,"error":{"category":"stale","message":"..."}}
```

## Architecture

```
cmd/llmlens/        serve | auth-start | export-creds entrypoints
cmd/smoketest/      Go binary running the smoke scenarios end-to-end
internal/cdp/       chromedp launch + attach wrapper
internal/perception/AXTree -> structured elements; HTML -> markdown
internal/session/   run-id, artifact tree, events.log writer
internal/tools/     the ten tools, refs, error taxonomy
internal/rpc/       shared dispatch, JSON-RPC and MCP transports
internal/credentials/auth-start watcher, cookie + storage capture/replay
```

Tool dispatch is in `internal/rpc/server.go::handle()`. Both transports
call into it. Adding a tool means adding one case there plus a descriptor
in `internal/rpc/mcp.go::toolDescriptors()`.

## Smoke scenarios

```bash
./bin/smoketest -scenario=snapshot-shape          # snapshot returns role=link with href
./bin/smoketest -scenario=maps-shape              # vision_recommended fires on canvas pages
./bin/smoketest -scenario=google-discovery        # navigate + snapshot + markdown
./bin/smoketest -scenario=google-interactive      # type + wait_for + snapshot
./bin/smoketest -scenario=creds-roundtrip      -profile=profiles/linkedin.json
./bin/smoketest -scenario=gmail-triage         -profile=profiles/gmail.json
./bin/smoketest -scenario=gmail-read-message   -profile=profiles/gmail.json
./bin/smoketest -scenario=twitter-triage       -profile=profiles/x.json
```

Agent-driven (manual, real LLM in the loop):

- `linkedin-ai-connections` — discovers AI-relevant 1st + 2nd order
  connections.
- `gmail-followups` — triages emails from a named sender into a
  structured follow-up list.
- `gmail-compose-send` — reads inbox, summarises, sends a digest to a
  user-controlled second inbox; verifies recipient before clicking Send.
- `twitter-engage` — drafts up to 5 thoughtful replies on the user's X
  feed; user confirms each draft before posting. Highest-trust task in
  the catalogue.

Run prompts and pass criteria are in `smoketests.md`.

## Project status

**Shipped:**

- 11-tool surface (navigate, back, forward, reload, close_browser,
  snapshot, click, type, screenshot, eval, wait_for) with stable
  error categories and self-healing browser state (stale chromedp
  context detected and relaunched on the next tool call)
- AXTree perception across same-origin sub-frames; link elements
  hydrated with `href`; sub-frame elements tagged with `frame`
- Snapshot signals for the agent: `auth_required` / `auth_hint` when
  the page is a login wall; `vision_recommended` / `vision_reason`
  when the page is canvas-rendered or AXTree-starved (Maps, Sheets,
  Figma, charts)
- HTML→markdown render + IE6-style `runs/<id>/<host>/<path>.{html,md}`
  artifact tree + JSONL `events.log` audit
- `auth-start` flow: open Chrome, watch login, capture cookies +
  per-origin storage; cross-domain federated auth (e.g. Google) handled
- `--profiles-dir` registry with `fsnotify` hot-reload — drop a bundle
  in, the running MCP server imports it
- JSON-RPC and MCP transports sharing one engine; lazy browser launch
  so registering the MCP server is cheap
- Deterministic smokes green: `snapshot-shape`, `google-discovery`,
  `google-interactive`, `creds-roundtrip` (LinkedIn), `gmail-triage`,
  `gmail-read-message`, `twitter-triage`, `maps-shape`
- Agent-driven scenarios validated end-to-end via MCP:
  `linkedin-ai-connections`, `gmail-followups`, `gmail-compose-send`,
  `twitter-engage`

## Active research areas

Open scope decisions — not missing features. Each entry has a concrete
*trigger* that would move it into the roadmap. The discipline that
made the shipped list right is the same discipline that says "don't
build these yet." See `PRD.md` for the design conversations behind
each one.

### Triggered when validated

These build the moment a real run produces the trigger condition.

- **`extract(schema)` tool** (PRD §2.3). *Trigger: agents repeatedly
  hand-write the same extraction pattern across runs.* Across four
  agent runs we've never seen this — Pattern A (LinkedIn cards) and
  Pattern B (Gmail row scraping) extract via existing primitives, and
  PRD §6 documents that eval is the right shape for tabular cases
  where snapshot's flat element list isn't enough.
- **Browser-extension transport** (PRD §3.3). Five triggers
  documented; none yet fired hard. Closest signal: Google session
  cookies rotate aggressively (~30 min lifetimes) — bundle re-capture
  is friction but not yet daily-friction-level.
- **Auto-mode vision escalation.** *Trigger: agent ignores the
  `vision_recommended` hint repeatedly on pages it should screenshot.*
  Today the hint is a signal; the agent decides. If we observe it
  being missed, we'd promote to a "snapshot includes a thumbnail when
  hint fires" mode.
- **Multi-tab handling.** *Trigger: a target task that needs more than
  one tab.* Compose-and-send happens in one tab; LinkedIn + Gmail +
  X all stay single-tab. None observed.
- **Cross-origin iframe traversal.** Same-origin works today via CDP.
  Cross-origin would require either CDP target attachment per frame
  or the browser-extension transport. *Trigger: a target where
  critical content lives in a cross-origin iframe.*

### Deployment-shape decisions

These depend on how llmlens gets shipped, not on capability gaps.

- **Stealth provider adapter** (Browserbase / Camoufox / Surfsky).
  *Trigger: deployment that requires hosted Chrome, residential
  proxies, or CAPTCHA solving* — i.e. SaaS, scraping at scale, or a
  target with bot-detection we can't pass with our bundle. For
  personal-agent use today, local Chrome is fit-for-purpose.
- **Prompt-injection threat model + `eval` allowlist mode.**
  *Trigger: shipping shape stabilises (CLI tool? library? hosted
  service?).* Each deployment posture has a different threat model.

### Out of scope by design

These will not be built. Listed so they don't accumulate as silent
technical debt.

- **Server-side OCR / image interpretation.** Claude (and any
  MCP-aware multimodal model) interprets images natively. We surface
  `screenshot()` and the `vision_recommended` hint; the agent
  reads pixels in its own context. Building OCR would duplicate
  upstream and rot.
- **Auto-attaching screenshots to every snapshot.** Token cost is
  too high; the Vardanyan paper itself recommends signalling, not
  auto-attaching.
- **Confidence scoring on perception elements.** Earlier PRD draft
  proposed it; couldn't define a meaningful score consumers would
  use. Dropped.

## Files of note

- `PRD.md` — product requirements, design rationale, deferral list
- `smoketests.md` — scenario catalogue, prep, pass criteria
- `runs/` — per-run artifacts (gitignored in practice)
- `profiles/` — credential bundles (gitignored; contains auth tokens)

## Troubleshooting

### MCP server stuck / orphaned

If `/mcp` in Claude Code shows `× failed` and reconnect doesn't help —
or if you've edited `~/.claude.json` and want a fully clean state — kill
the host process and any orphaned llmlens server, then relaunch.

```bash
# If hosted via Zed, quit the editor first
osascript -e 'quit app "Zed"'

# Kill the Claude Code adapter and any llmlens serve still running
pkill -9 -f "claude-agent-acp"
pkill -9 -f "claude-agent-sdk"
pkill -9 -f "bin/llmlens serve"

# Reopen
open -a Zed /Users/nickglynn/Projects/llmlens
```

For Claude Code in a terminal: just close the conversation and start a
new one — the MCP server gets relaunched fresh.

### Bundle was working, now bouncing to login

Some sites (notably Google) rotate session cookies aggressively. A
working bundle can go stale within ~30 minutes. Re-run `auth-start`;
the watcher hot-imports the new bundle into the live browser.

```bash
./bin/llmlens auth-start -domain=mail.google.com -out=profiles/gmail.json
```

If this happens daily for a target site, that's PRD §3.3 trigger #1
firing — would justify the Chrome-extension transport.

### `auth_required: true` mid-task

The agent landed on a login wall it couldn't bypass. Snapshot's
`auth_hint` field carries the action: usually "run `llmlens auth-start
--domain=X`". Do that; the watcher imports; ask the agent to retry.

### Eval errors look like "Uncaught" with no detail

Should be fixed (Phase 1.6 lifted the truncation cap to 4000 chars and
uses CDP's rich `ExceptionDetails.Error()`). If you still see this,
check that you're running the latest binary — the MCP server caches
the binary path at registration; re-running `go build` doesn't restart
it. Use the troubleshooting kill+relaunch above.

### Live-debugging an MCP run

Set `LLMLENS_DEBUG_LOG` to a file path before launching Claude Code,
then `tail -f` it from another terminal:

```bash
export LLMLENS_DEBUG_LOG=/tmp/llmlens.log
# (re-)launch Claude Code so the MCP server inherits the env var

# In another terminal:
tail -f /tmp/llmlens.log
```

The log captures lifecycle (server start, browser launch/close,
stale-context relaunches), tool calls (entry + exit with duration +
error category), registry imports + hot-reloads, and watcher events.
Plain text, one event per line, no rotation — `truncate -s 0
$LLMLENS_DEBUG_LOG` to clear between runs.

When the env var is unset, debug logging is a no-op. Safe to leave the
plumbing in place for production users; only those who set the var pay
the file-write cost.

## License

Not yet specified.
