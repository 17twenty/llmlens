# Product Requirements Document: LLMLens

## 1. Project Overview
LLMLens is a production-grade infrastructure for LLM browser harnesses. Unlike traditional scraping tools or high-level automation wrappers, LLMLens is built for reliability, efficiency, and granular control. By bypassing high-level abstractions like Playwright or Puppeteer and binding to the Chrome DevTools Protocol (CDP) through a thin Go layer, we keep the tool surface minimal, the token footprint low, and the agent in control.

**Status (2026-04 development snapshot):** smokes pass against Google search, LinkedIn (search + feed), and Gmail (inbox + iframe-rendered message bodies). Agent-driven scenarios validated end-to-end against LinkedIn and Gmail via MCP. PRD §3.3 extension triggers monitored but not yet fired.

## 2. Technical Architecture

### 2.1 Direct CDP Integration
The core of LLMLens is a thin Go layer over the Chrome DevTools Protocol. We use `chromedp` as the CDP client (well-maintained, CDP-native, no Playwright/Puppeteer veneer) and isolate it behind `internal/cdp` so it can be swapped for a hand-rolled websocket layer if upstream churn ever bites. This architectural choice lets us:
*   Expose exactly the surface area needed by the harness.
*   Control the resource and token footprint of every tool definition.
*   Avoid the overhead and "2023-era" detection signatures associated with heavy automation libraries.

### 2.2 Perception Layer: The Escalation Ladder
LLMLens adopts the "Cellar (CEL)" design philosophy for page perception. We prioritise cheap, structured data and escalate only when necessary.

1.  **Accessibility Tree (default, shipped).** AXTree is the primary perception source — the most semantic and token-efficient representation of a page. We traverse **every same-origin sub-frame** so iframe content (Gmail message bodies, embedded widgets) appears alongside main-frame elements; cross-origin sub-frames whose AXTree refuses are skipped per-frame.
2.  **Structured JSON (shipped).** Each element carries `role`, `name`, `value`, `description`, `focusable`, `disabled`. Elements with `role=link` are hydrated with `href` (DescribeNode lookup post-AXTree-walk) so URL-collection tasks don't need `eval`. Sub-frame elements carry their `frame` id. Snapshot also flags `auth_required` + `auth_hint` when the page heuristically looks like a login wall.
3.  **Markdown render (shipped).** `snapshot(include_markdown=true)` returns an HTML→Markdown render of the main frame; `save_artifacts=true` writes html + md + screenshots into `runs/<run-id>/<host>/<path>.{html,md,png}` (the IE6-style cache layout).
4.  **Vision Escalation (deferred).** `screenshot()` exists; an automatic *gate* (snapshot heuristic decides "I should also return a PNG") is unbuilt. Per Arxiv 2511.19477 ("Building Browser Agents," Vardanyan), hybrid AXTree + selective vision is the recommended posture; we'll wire the gate when a real failure justifies it. Per-element confidence scores referenced in earlier drafts have been dropped — we couldn't define a meaningful score consumers would use.

### 2.3 Sharp Tool Surface
To maintain high agent decision quality and maximize context window efficiency, the harness exposes a minimal set of around eleven tools. We follow the "minimal floor" philosophy seen in bash-CDP approaches.

*   `navigate(url)`: Direct browser navigation.
*   `back` / `forward` / `reload`: History controls.
*   `close_browser`: Shut down Chrome; next tool call lazy-relaunches.
*   `snapshot()`: Retrieve the current structured perception layer. Elements with `role=link` include `href`, eliminating the need for `eval` on URL-collection tasks.
*   `click(ref)`: Interact with an element via reference ID.
*   `type(ref, text)`: Input text into a specific element.
*   `screenshot()`: Capture visual state for VLM escalation.
*   `eval(js)`: The "escape hatch" for custom execution.
*   `wait_for(condition)`: Deterministic waiting for page states.

**Deferred:** `extract(schema)` — kept on the roadmap; will design once we have real agent runs showing what shapes of data the LLM most often gropes for.

## 3. Operational Strategy

### 3.1 Stealth and Infrastructure
Stealth is treated as a separate, swappable concern rather than a core harness feature. This prevents the "multi-engineer-year" commitment of building in-house evasion.
*   **Dev/Test Environment:** Runs against vanilla Chromium for speed and transparency.
*   **Production Environment:** Utilizes a swappable browser provider (e.g., Browserbase, Camofox, or Surfsky) to handle residential proxies, fingerprint randomization, and Turnstile solving.

### 3.2 Session Management and Auth
LLMLens treats authentication as a first-class operational concern, not a retrofit:
*   **`auth-start --domain=X --out=Y`**: opens a dedicated headed Chrome to the target site, watches the URL for the user to finish logging in (URL stable on a non-login pattern for ≥3s), then captures cookies + per-origin web storage. The temp profile is naturally scoped to login-flow cookies, so capture is unfiltered — federated auth like `accounts.google.com` ↔ `mail.google.com` works without extra config.
*   **`serve --profiles-dir=...`**: registry of `*.json` bundles, all imported on browser launch. fsnotify watches the directory and hot-reloads new/updated bundles into the running browser — drop a file in, the agent's next snapshot sees the auth.
*   **`export-creds --remote=...`**: legacy / advanced path against an externally-managed Chrome (e.g. one running with `--remote-debugging-port=9222`). Domain-filtered for privacy.
*   **Login-wall detection**: `snapshot()` returns `auth_required: true` + an actionable `auth_hint` when the agent lands on a login form. The agent surfaces the hint to the human; the human runs `auth-start`; the watcher imports the new bundle. No agent-side `auth_start` tool is exposed (phishing-surface concern).

### 3.3 Browser Extension Path (deferred, not declined)

We are not philosophically opposed to shipping a Chrome extension as a second
attachment surface alongside CDP launch/attach. We will build one when at
least one of the following is true for a target use case:

1.  **Cookie rotation is fast enough that re-running `auth-start` becomes
    daily operator friction.** Observed in early Gmail testing: a 22-cookie
    bundle that passed `gmail-triage` minutes after capture failed the same
    smoke ~30 minutes later, bouncing back to `accounts.google.com/signin`.
    Google's session cookies are aggressive about expiry / rotation. Not yet
    daily-friction-level for our usage, but a real signal that bundle
    longevity is bounded for some sites.
2.  **A target site flags imported-cookie sessions even from headed Chrome.**
    LinkedIn identity-verification loops are the canonical example. Not yet
    triggered in our runs (LinkedIn auth is happily long-lived once captured).
3.  **The agent should run inside the user's normal Chrome alongside human
    browsing**, not in a separate window. UX preference for "agent rides
    along," not yet a load-bearing requirement.
4.  **The site uses hardware-bound auth (WebAuthn / passkeys)** that cannot
    be exported. Not yet hit; Gmail's password+TOTP path captures cleanly.
5.  **Cross-origin iframe content is unreachable via CDP from the parent
    context.** AXTree-traversal across same-origin frames works; if a target
    surface puts critical content in a cross-origin iframe whose AXTree
    refuses to load, an in-page extension content script is the natural
    workaround.

Architectural sketch when we do: a thin extension that uses
`chrome.debugger.attach` for the full CDP surface and bridges to the
existing LLMLens Go process via native messaging or a localhost websocket.
The internal tool surface stays unchanged — the extension is a transport,
not a new abstraction.

Until one of those triggers fires hard, the `auth-start` + `--profiles-dir`
+ hot-reload path covers the same problems for less complexity.

## 4. Testing and Validation
Reliability is managed through `smoketests.md`. Two layers:

**Deterministic (Go binary, fast, CI-safe):**
*   `snapshot-shape` — example.com sanity guard for `Element` schema (role, href, frame).
*   `google-discovery` — navigate + snapshot + markdown end-to-end.
*   `google-interactive` — type into a real input + wait_for + snapshot.
*   `creds-roundtrip` — bundle import drives an authenticated LinkedIn `/feed/`.
*   `gmail-triage` — bundle import drives an authenticated Gmail inbox.
*   `gmail-read-message` — open an email; assert iframe traversal returns sub-frame elements.

**Agent-driven (manual, real LLM in the loop):**
*   `linkedin-ai-connections` — Claude over MCP discovers 1st + 2nd order LinkedIn connections in AI/ML.
*   `gmail-followups` — Claude triages emails from a named sender into a structured follow-up list.
*   `gmail-compose-send` — Claude reads the past week of inbox, composes a digest, sends to a user-controlled second inbox; verifies recipient before clicking Send.

## 5. Reference Materials
*   **Stagehand:** For clean primitives and CDP-native architecture.
*   **Browser-use:** For the harness loop architecture.
*   **Cellar (CEL):** For the perception layer design.
*   **Mario Zechner (bash-CDP):** For the minimalist tool surface philosophy.
*   **Vardanyan, "Building Browser Agents: Architecture, Security, and Practical Solutions"** (Arxiv 2511.19477): hybrid AXTree + selective vision posture; framing for the §2.2 escalation ladder.

## 6. Observed Tool-Usage Patterns

The shape of an agent's tool usage is determined by the *target site's UI design*, not by the agent's training preference. Three patterns emerged across today's real agent runs and are worth recording so future design decisions stay grounded.

### Pattern A — Read-only structured tasks on role-based UIs
**Example:** LinkedIn AI-connections (15 connection cards extracted to CSV).

| navigate | snapshot | click | type | eval | wait_for | total |
|---|---|---|---|---|---|---|
| 2 | 3 | 1 | 0 | 1¹ | 1 | **8** |

¹ The single eval was a `Boolean(...)` predicate inside a wait_for — not data extraction.

LinkedIn search-result cards are emitted as `role=link` elements with full headline + name + mutual-connections text in the `name` field. Once snapshot's link elements include `href` (Phase 1.6 fix), the agent has everything it needs in a single tool call. Snapshot-driven, very efficient.

### Pattern B — Tabular extraction on custom-DOM lists
**Example:** Gmail Karl follow-ups (8 items from `from:karl@greenthread.ai`).

| navigate | snapshot | click | type | eval | wait_for | total |
|---|---|---|---|---|---|---|
| 2 | 1 | 0 | 0 | 5 (2 extraction + 3 wait predicates) | 3 | **11** |

Gmail's email rows are `<tr role="row">` with `<td role="gridcell">` children — neither is in our `surfacedRoles` filter, so they don't appear in snapshot output. The agent reaches for `eval` with `querySelectorAll('tr.zA')` to pull structured per-row fields. Two extraction evals (each ~3ms) deliver the data; the rest are `Boolean(...)` predicates feeding `wait_for(js:...)`. **Eval is the right primitive here** — adding row/gridcell-shaped hierarchy to snapshot would inflate token cost without making the agent meaningfully faster.

### Pattern C — Multi-step actions on heavily-custom UIs
**Example:** Gmail compose-send (read 7 days, summarise, send digest to second inbox).

| navigate | snapshot | click | type | eval | wait_for | total |
|---|---|---|---|---|---|---|
| 1 | **0** | 0 | 0 | **81** | 3 | **85** |

Gmail's compose dialog is a stack of `contenteditable` divs with custom keyboard handling, Trusted Types CSP enforcement, and timing-sensitive autocomplete. None of it has stable ARIA roles our snapshot would surface. The agent went 100% eval — and **succeeded** (email arrived at the second inbox, send-toast confirmed, full digest written to disk).

Notable detail: ~20 of the 81 evals were a `Boolean(!!document.querySelector(...))` polling burst after a `wait_for(js:...)` timed out at 10s. The compose dialog opens slowly enough that the default timeout was tight. Future tweak: bump default `wait_for` to 15–30s or sharpen the tool description so agents pass `timeout_ms` higher for known-slow modals. Marginal — the run still worked.

### Implication for design

- **Snapshot** is for *perception*: read structure, find anchors, collect URLs. Best when the site has thoughtful ARIA. Agents will reach for it naturally when it's useful.
- **Eval** is for *action* on custom UIs and *bulk extraction* on role-poor markup. Don't try to bend snapshot into doing this — the cost of generality ruins the token economy of the common case.
- **`auth_required` on snapshot** is the third primitive — a structured signal the agent uses to *abort* and surface a hint, rather than wandering further.

The PRD's "AXTree-first" thesis (§2.2) holds; what we learned is that "first" doesn't mean "exclusive." Eval as escape hatch is load-bearing for the action surface, and that's the right shape — not a regression.
