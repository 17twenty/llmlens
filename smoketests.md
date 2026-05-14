# LLMLens Smoketests

Reliability is managed here. Each scenario describes its purpose, prep steps,
how to run it, and the pass criteria. Scenarios run via the `cmd/smoketest`
binary against a real, locally-launched (or attached) Chrome.

```bash
go build -o bin/llmlens     ./cmd/llmlens
go build -o bin/smoketest   ./cmd/smoketest
```

Artifacts (per-page HTML + markdown + screenshots) land under `runs/<run-id>/`
in an IE6-style host/path tree.

## Scenario index

| ID                    | Purpose                                       | Auth | Default in CI |
|-----------------------|-----------------------------------------------|------|---------------|
| `snapshot-shape`      | Guard: snapshot returns role=link with href   | none | yes           |
| `maps-shape`          | Guard: vision_recommended fires on canvas pgs | none | yes           |
| `google-discovery`    | Validate navigate / snapshot / markdown       | none | yes           |
| `google-interactive`  | Validate snapshot / type / wait_for           | none | yes           |
| `linkedin-attached`   | Validate attached-session + cookies for auth  | yes  | manual        |
| `creds-roundtrip`     | Validate auth-start → serve --profile reuse   | yes  | manual        |
| `gmail-triage`        | Validate Gmail bundle → authenticated inbox   | yes  | manual        |
| `gmail-read-message`  | Validate iframe traversal (message body)      | yes  | manual        |
| `twitter-triage`      | Validate X bundle → authenticated home feed   | yes  | manual        |

---

## snapshot-shape

**Purpose.** Cheap regression guard for the `Element` schema in
`internal/perception/snapshot.go`. If we widen `surfacedRoles`, change how
`Href` is hydrated, or otherwise alter the snapshot output, this catches it
against a stable fixture without auth or real scraping.

**Prep.** None.

**Run.**

```bash
./bin/smoketest -scenario=snapshot-shape -headless=true
```

**Pass criteria.**
1. `https://example.com` snapshot returns ≥ 2 elements.
2. At least one element has `role == "link"`.
3. That element's `Href` is non-empty and starts with `http://` or `https://`.

If link hydration regresses (e.g. `cdpdom.DescribeNode` returns errors and
gets silently swallowed), `Href` becomes empty and this fails fast.

## maps-shape

**Purpose.** Regression guard for the canvas-detection probe in
`internal/perception/snapshot.go::detectVisionNeed`. Google Maps is the
canonical canvas-rendered surface — the actual map and its annotations
live on a full-viewport `<canvas>` that the AXTree never exposes. If
this scenario stops setting `vision_recommended: true`, agents will
silently lose the ability to recognise canvas pages and reach for
`screenshot()`.

**Prep.** None. No auth, public URL.

**Run.**

```bash
./bin/smoketest -scenario=maps-shape
```

**Pass criteria** (enforced):
1. Snapshot of `https://maps.google.com` sets
   `vision_recommended: true`.
2. `vision_reason` mentions `canvas` or `axtree` (covers both the
   strong canvas-coverage trigger and the weak AXTree-starvation
   fallback).

**Failure-mode interpretation:**
- Hint failed to fire — either the canvas heuristic regressed (DOM
  query, viewport-coverage maths) or Maps changed its rendering
  strategy. Inspect saved artifacts; re-tune the threshold.
- False-positive sibling failures (other smokes start flagging vision
  on pages that don't need it) — tighten the 20% viewport threshold
  or filter out tracking canvases.

## google-discovery

**Purpose.** End-to-end smoke for `navigate` → `snapshot(markdown)`. Cheapest
fail-detector — if this fails, perception is broken.

**Prep.** None beyond local Chrome at `/Applications/Google Chrome.app`. To
avoid the EU consent gate on cold profiles, persist a profile dir:

```bash
mkdir -p .smokeprofile
```

Open Chrome once with that profile dir, dismiss any consent dialog, close it.

**Run.**

```bash
./bin/smoketest -scenario=google-discovery -user-data-dir=$PWD/.smokeprofile
```

**Pass criteria.**
1. Navigate succeeds; resulting URL is not a Google consent redirect.
2. Snapshot returns ≥ 50 elements (sanity floor).
3. Rendered markdown contains the literal string `Nick Glynn`
   (case-insensitive).
4. `runs/<run-id>/www.google.com/search_q_q-Nick+Glynn*.{html,md}` exists.

**Failure modes worth watching.**
- Consent gate redirect → use a persisted user-data-dir.
- "captcha" or "unusual traffic" page → Google has flagged the IP. Rerun later
  or escalate to a stealth provider (post-MVP).
- Empty markdown → likely AXTree captured pre-render; bump the post-load sleep
  in `cmd/smoketest/main.go` or wait on a results-specific selector instead.

---

## google-interactive

**Purpose.** Validates the action surface (`type`, `wait_for`) by typing into
the search box and submitting with Enter, rather than building a URL.

**Prep.** Same as google-discovery (persisted user-data-dir recommended).

**Run.**

```bash
./bin/smoketest -scenario=google-interactive -user-data-dir=$PWD/.smokeprofile
```

**Pass criteria.**
1. `snapshot()` on the homepage exposes a ref with role `combobox` /
   `searchbox` / `textbox`.
2. `type(ref, "Nick Glynn", press_enter=true)` advances the URL to a
   `/search?q=...` page within 15s.
3. The follow-up snapshot's markdown contains `Nick Glynn`.

**Notes.** This is more brittle than `google-discovery` because Google
homepage markup churns. If the role detection misses, broaden the heuristic
in `runGoogleInteractive` or fall back to a CSS selector via `eval`.

---

## linkedin-attached

**Purpose.** Validates attached-session mode against a real auth-bearing site.
Confirms LLMLens can drive your actually-logged-in browser without re-login.

**Prep.**

1. Quit Chrome.
2. Launch Chrome with the remote-debugging port enabled, pointed at your
   normal profile so you stay signed in:

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 \
  --user-data-dir="$HOME/Library/Application Support/Google/Chrome/Default-CDP"
```

   (First time, sign in to LinkedIn in that window.)

3. From a separate terminal, drive it via the JSON-RPC server in attach mode:

```bash
./bin/llmlens serve -mode=attach -remote=http://localhost:9222
```

Then, on stdin, send (one JSON object per line):

```json
{"jsonrpc":"2.0","id":1,"method":"navigate","params":{"url":"https://www.linkedin.com/feed/"}}
{"jsonrpc":"2.0","id":2,"method":"wait_for","params":{"condition":"load","timeout_ms":15000}}
{"jsonrpc":"2.0","id":3,"method":"snapshot","params":{"include_markdown":true,"save_artifacts":true}}
```

**Pass criteria.**
1. Resulting URL stays on `/feed/` (not redirected to `/login`).
2. Snapshot exposes elements whose names include the user's display name or
   "Home" / "My Network" — i.e. authenticated chrome.
3. Saved markdown under `runs/<run-id>/www.linkedin.com/feed.md` is non-empty.

---

## creds-roundtrip

**Purpose.** Validates `auth-start` → `serve --profile` (or `smoketest
-scenario=creds-roundtrip`) so the agent can run in a *fresh, ephemeral*
Chrome that picks up your real auth without re-typing a password.

**Prep.** None. `auth-start` handles browser launch, login watching, and
export in one command.

**Run.**

```bash
mkdir -p profiles
./bin/llmlens auth-start -domain=linkedin.com -out=profiles/linkedin.json
# A Chrome window opens. Log in. Capture fires automatically once the URL
# stabilises on a non-login page (default 3s settle).
```

Then verify the bundle drives an authenticated session:

```bash
./bin/smoketest -scenario=creds-roundtrip -profile=profiles/linkedin.json
```

**Pass criteria** (enforced by the smoketest):
1. The fresh launched Chrome lands on `https://www.linkedin.com/feed/` and
   does not bounce to `/login`, `/uas/login`, `/authwall`, or
   `/checkpoint/`.
2. Captured snapshot includes ≥ 2 of the canonical authenticated nav
   markers: `my network`, `messaging`, `notifications`, `/feed/follows`,
   `in/`.

**Why it matters.** This is the demo that distinguishes LLMLens from
"automate a fresh browser" tools — you log in once, by hand, and the agent
inherits the session in any subsequent run.

**Failure-mode interpretation.**
- `AUTH FAILURE: bounced to login` — the bundle isn't being honoured. Likely
  cookie domain mismatch or `Network.SetCookies` was called too late
  relative to the navigation. Inspect `runs/<id>/events.log` and the
  resulting `runs/<id>/www.linkedin.com/feed.html` artifact.
- `VERIFICATION CHALLENGE: LinkedIn flagged the new browser instance` —
  device-fingerprint divergence between auth-start Chrome and serve Chrome.
  This is the documented trigger for the extension path (PRD §3.3).
- `AMBIGUOUS` — feed loaded but partial. Often a slow JS-rendered page;
  bumping the post-load settle in the smoketest is the usual fix.

## creds-roundtrip-attached (legacy / advanced)

The original `export-creds` flow against an externally-managed Chrome stays
supported for CI and headless-export scenarios:

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 \
  --user-data-dir="$HOME/Library/Application Support/Google/Chrome/llmlens-cdp"
# log in manually, then in another terminal:
./bin/llmlens export-creds -domain=linkedin.com -out=profiles/linkedin.json
```

Most users should prefer `auth-start`.

---

## gmail-triage

**Purpose.** Validate that an `auth-start`-captured Gmail bundle drives an
authenticated inbox in a fresh launched browser. The deterministic
prerequisite for the agent-driven `gmail-followups` scenario below.

**Prep.** Use a *persistent* `-user-data-dir` so Chrome's device
fingerprint survives into `serve`. Without this, the bundle has a
~30-minute useful lifetime (Google rotates session cookies aggressively
when it can't match the profile fingerprint).

```bash
./bin/llmlens auth-start \
  -domain=mail.google.com \
  -out=profiles/gmail.json \
  -user-data-dir=profiles/.chrome-google
```

A Chrome window opens; sign in to your Google account. If a passkey or
"verify it's you" challenge appears, complete it in the window. With the
persistent profile dir, those challenges typically appear once and the
result sticks across future sessions.

**Run.** Match the `-user-data-dir` on the smoketest binary too:

```bash
./bin/smoketest -scenario=gmail-triage \
  -profile=profiles/gmail.json \
  -user-data-dir=profiles/.chrome-google
```

(For non-Google sites, `-user-data-dir` is optional — ephemeral default
works fine. It's the Google-specific fingerprint dance that needs it.)

**Pass criteria** (enforced):
1. Snapshot does not flag `auth_required: true`.
2. Final URL stays on `mail.google.com` (not `accounts.google.com/signin`).
3. Snapshot returns ≥30 elements (inbox is dense; below this means the
   page didn't render).
4. Page title contains "Inbox", "Mail", or "Gmail".

**Soft signal:** "Compose" button visible in elements list. If absent,
the Gmail UI may have shifted; inspect the saved artifacts.

**Failure-mode interpretation:**
- `AUTH WALL HIT` — the imported cookies aren't enough for Gmail.
  **This is a documented PRD §3.3 trigger** justifying the extension
  path. Capture the run dir, file the failure mode, decide on next
  steps.
- `AUTH FAILURE: bounced to Google sign-in` — same root cause as above
  but Google chose to fully redirect rather than show a verification
  challenge.
- `expected ≥30 elements` — Gmail loaded but JS hadn't finished. Bump
  the post-load sleep in `runGmailTriage` and retry.

## gmail-read-message

**Purpose.** Deterministic guard for iframe traversal. Gmail puts message
bodies in a sandboxed sub-frame; without `collectAXTreesAllFrames`,
snapshot returns subjects + senders but no body content. This scenario
opens the most recent inbox message and asserts the snapshot includes
elements attributed to a sub-frame.

**Prep.** A working `profiles/gmail.json` (i.e. `gmail-triage` PASS).

**Run.**

```bash
./bin/smoketest -scenario=gmail-read-message -profile=profiles/gmail.json
```

**Pass criteria** (enforced):
1. Inbox renders without auth wall (sanity check).
2. The first email row is found and clicked.
3. The message detail view's snapshot has ≥ 1 distinct sub-frame
   contributing ≥ 2 elements (sub-frame perception is live).

**Failure-mode interpretation:**
- `expected ≥1 sub-frame in message detail view, found 0` —
  `collectAXTreesAllFrames` walked the frame tree but every sub-frame
  refused. Likely cross-origin sandboxing on Gmail's body iframe; CDP
  can't see in. **This is the iframe-flavoured §3.3 trigger** — would
  justify the extension transport.
- `only N frame-attributed elements` — a frame exists and is reachable
  but its AXTree is empty. Could mean the iframe content hasn't loaded
  yet (bump the post-click sleep) or its accessibility tree is
  intentionally suppressed.
- `no email row found in inbox snapshot` — the inbox heuristic (look
  for `role=link` whose `href` contains `#inbox/`) is brittle to Gmail
  UI changes. Inspect saved artifacts; widen the heuristic.

## gmail-followups (agent-driven, manual)

**Purpose.** End-to-end demo of llmlens against an email surface — read
inbox, search, judge, write notes. The Karl-task variant proposed by the
user; tests llmlens-as-agent-harness on a richer surface than LinkedIn
search.

**Prep.**

1. Run `gmail-triage` (above) and confirm it passes. Don't proceed if
   it fails — the agent run will fail the same way and waste your time.
2. **No additional MCP registration needed.** The single `llmlens` MCP
   server points at `profiles/`, which now contains both
   `linkedin.json` and `gmail.json`. Cookies for both domains are
   imported on browser launch.
3. If your Claude Code conversation was open before you ran
   `auth-start`, the watcher will have already hot-reloaded the new
   `gmail.json`. If it wasn't running, just open a new conversation —
   the file gets picked up on launch.

**Run.** In a new conversation in this directory:

> Use the llmlens tools to find emails from Karl in my inbox that need
> following up. For each: subject, sender email, date received, and a
> short note explaining why it needs follow-up. Save the results to
> `runs/<run-id>/karl-followups.md`. Use Gmail search operators where
> useful (`from:karl`, `is:unread`, etc.). If a snapshot reports
> `auth_required: true`, stop and tell me — don't try to bypass.

**Pass criteria.**
1. Agent does not hit `auth_required: true` (would indicate the bundle
   broke).
2. Agent uses Gmail search operators at least once (URL contains
   `#search/` or `#advanced-search/`).
3. Output file exists with structured entries: subject, sender, date,
   note.
4. Tool-frequency analysis: `snapshot` count > `eval` count (sanity
   check that the LinkedIn lesson held; if not, the snapshot tool
   description for Gmail-shaped UIs needs sharpening).
5. Agent surfaces "no follow-ups needed" gracefully if there are
   genuinely no matching messages from any Karl.

**Failure modes worth watching:**
- **Iframe gaps** — Gmail's compose modal is iframe-bound and may
  surface as "I can read the message list but cannot read the message
  body." This is the trigger for Phase 2 cross-frame snapshot.
- **Virtualised inbox** — list rows beyond the viewport don't appear
  in AXTree until scrolled. May need a scroll-shaped tool addition or
  agent-driven scroll-via-eval.
- **Search operator misuse** — `from:karl` is broad; agent might miss
  a "Karl Surname" who signs differently. That's a prompt-engineering
  issue, not a harness gap.
- **Auth wall mid-task** — if Google forces re-auth, the agent should
  see `auth_required: true` on the next snapshot and stop cleanly.

## gmail-compose-send (agent-driven, manual)

**Purpose.** End-to-end test of the **action surface** — read recent
emails, summarise outstanding items, compose a digest, and send to a
user-controlled second inbox. The most ambitious agent task in the
catalogue: real action with a verifiable receipt. Tests iframe traversal
(message bodies), multi-step UI interaction (Compose modal), and
contenteditable typing.

**Prep.**
1. `profiles/gmail.json` exists and `gmail-triage` PASSes.
2. `gmail-read-message` PASSes (iframe traversal works).
3. The user controls the recipient inbox (`nick@curiola.com` in the
   reference run) so the send is verifiable and reversible.

**Run.** In a fresh Claude Code conversation in this directory:

> Look at emails in my Gmail inbox from the past 7 days. Identify
> outstanding items needing my attention (forwarded action items,
> @-mentions of me, direct requests). Compose a digest email **to
> `nick@curiola.com` only** with subject `Weekly outstanding items —
> <date range>` and a plaintext body listing the items. Show me the
> draft in this conversation before clicking Send. Verify the To:
> field exactly matches `nick@curiola.com` with no other recipients,
> then click Send. Save a copy of the digest to
> `runs/<run-id>/weekly-digest.md`.

**Pass criteria.**
1. Email arrives at the recipient inbox (user-verifiable).
2. `runs/<id>/weekly-digest.md` exists and matches what was sent.
3. `events.log` shows no `auth_required: true` and no `stale` errors
   that weren't recovered from.
4. Tool-frequency analysis: snapshot ≥ 1 per page transition; iframe
   traversal evident (snapshot output includes elements with
   `frame: <id>`).

**Failure modes worth watching.**
- **Iframe gap on message body** — pre-empted by `gmail-read-message`
  passing. If this still fires, something changed.
- **Compose pane** — modern Gmail uses main-frame contenteditable for
  compose. If it surprises us with an iframe, Step 1 covers it.
- **contenteditable typing** — `Input.InsertText` should work for
  contenteditable like `<input>`/`<textarea>`. If not, the type tool
  needs a fallback.
- **Stale refs after Compose modal opens** — clicking Compose mutates
  the DOM significantly. Agent must re-snapshot before typing into
  To/Subject/Body. Stale-ref errors here would be informative.
- **Send confirmation dialog** — Gmail occasionally prompts on missing
  subject or recipient warnings. Agent must handle the modal.

**Safety notes (prompt-enforced).**
- Recipient must be exactly the configured address. No CCs, BCCs, or
  alternates.
- Plaintext body, no HTML the agent didn't intend.
- Show the draft to the user in conversation before Send.
- The reference run uses a user-controlled second inbox; sending isn't
  risky. The "verify-then-send" pattern is good practice for when the
  recipient is someone else.

## twitter-triage

**Purpose.** Validate that an `auth-start`-captured X (Twitter) bundle drives
an authenticated home feed in a fresh launched browser. X's bot detection
is the most aggressive of the surfaces we test against — this is the
canonical smoke for **PRD §3.3 trigger #2** if it ever fires.

**Prep.**

```bash
./bin/llmlens auth-start -domain=x.com -out=profiles/x.json
```

A Chrome window opens; sign in. Push through any "Verify it's you" or
phone-number challenge. X may redirect via `/i/flow/login` and a
TOTP/SMS step; the watcher captures only after the URL settles on a
non-login page (typically `/home`).

**Run.**

```bash
./bin/smoketest -scenario=twitter-triage -profile=profiles/x.json
```

**Pass criteria** (enforced):
1. Snapshot does not flag `auth_required: true`.
2. Final URL stays on `x.com` (or `twitter.com`) — not `/i/flow/login`.
3. Snapshot returns ≥30 elements (X home is dense; below this means
   the timeline didn't render).
4. At least one of these soft markers visible: a Post button (compose),
   a Home nav element, or the "What is happening?" compose placeholder.

**Failure-mode interpretation:**
- `AUTH WALL HIT` / `AUTH FAILURE: bounced to login flow` — bundle
  insufficient. X may have rotated the session, or its anti-automation
  flagged the new browser. **PRD §3.3 trigger #2 candidate.** Re-run
  `auth-start` first; if it still fails, document and consider extension.
- `expected ≥30 elements` — feed didn't render. Bump the post-load
  sleep in `runTwitterTriage` and inspect saved artifacts.
- `no auth markers found` — feed rendered but UI shifted. Inspect
  artifacts; widen the marker heuristic.

## twitter-engage (agent-driven, manual)

**Purpose.** Highest-trust agent task in the catalogue — public,
identity-attached actions on a sensitive surface. Tests whether the
harness can drive X reliably *and* whether the prompt-enforced safety
patterns (draft-then-confirm, hard cap on actions) hold up in practice.

**Prep.**
1. `profiles/x.json` exists and `twitter-triage` PASSes.
2. The bundle is fresh — X session lifetimes are short.

**Run.** In a fresh Claude Code conversation in this directory:

> Open my X (twitter.com) home feed. Scroll a bit to load posts and
> identify up to **5** high-value people I might thoughtfully reply to.
> "High-value" means substantive accounts (journalists, public
> intellectuals, builders, established voices) — not shitposting,
> rage-bait, or fringe accounts. For each candidate, draft a reply
> that:
>
> - Is genuinely thoughtful — adds something to the conversation,
>   doesn't just agree, doesn't attack.
> - Engages with the *specific* post and the *specific* person, not a
>   generic comment.
> - Reads as a non-crazy, Australian centre-right voice if a political
>   frame is needed at all (pro-business, fiscally conservative,
>   institutionally respectful, pragmatic — not US-Republican
>   culture-war coded).
> - Is concise (X's character limit, no thread spam).
>
> **Show me each draft in this conversation before you post it.** Wait
> for me to say "post" before clicking the reply Post button. If I
> say "skip", move on to the next candidate. Hard stop after 5
> successful posts. Save the running log of decisions and posted
> replies to `runs/<run-id>/x-engagements.md`.
>
> If `auth_required: true` appears on any snapshot, stop and tell me.
> If you see a rate-limit or "Something went wrong" interstitial,
> stop and tell me.

**Pass criteria.**
1. ≤ 5 replies posted, each preceded by an explicit user "post"
   confirmation in the conversation.
2. `runs/<id>/x-engagements.md` exists with the candidate post, the
   draft reply, and the disposition (posted/skipped) for each.
3. No `auth_required: true` mid-task.
4. No replies posted to fringe / rage-bait accounts.

**Failure modes worth watching.**
- **Bot-detection challenge** — X may inject a captcha/Arkose challenge
  mid-session. Agent should stop and surface the snapshot.
- **Compose UI is heavily custom** (cf. Gmail compose-send pattern C in
  PRD §6) — expect eval-heavy execution for the typing + clicking
  Reply flow. That's fine; agent should still work.
- **Stale refs after scroll** — virtualised timeline detaches DOM nodes
  beyond the viewport. Agent must re-snapshot before clicking a row
  it identified earlier.
- **Rate limit / "unable to post"** — X enforces cooldowns between
  rapid replies. Pace ≥30s between posts; the prompt's "wait for me
  to say post" naturally handles this.

**Safety notes (prompt-enforced).**
- The user is the gate on every individual post. The agent never
  posts without explicit confirmation.
- Hard cap at 5; the prompt enforces this and the agent should
  refuse to continue past it.
- Centre-right framing is *the user's view* — the agent is helping
  them write, not performing political speech of its own. This is no
  different from drafting an email on the user's behalf in their
  voice.
- If the agent ever feels uncertain about a draft, it should ask
  rather than post.

## linkedin-ai-connections (agent-driven, manual)

**Purpose.** End-to-end demo of LLMLens as an agent harness. An LLM (Claude
in Claude Code, talking via MCP) drives the tool surface to discover 1st
and 2nd order LinkedIn connections working in AI/ML. This is the test that
says "the harness is actually agent-shaped," not "we can drive Chrome."

**Prep.**

1. Capture LinkedIn auth (one-time):

   ```bash
   ./bin/llmlens auth-start -domain=linkedin.com -out=profiles/linkedin.json
   ```

2. Register llmlens as an MCP server with Claude Code. From the project
   root:

   ```bash
   claude mcp add llmlens \
     "$PWD/bin/llmlens" \
     -- serve --protocol=mcp --profile="$PWD/profiles/linkedin.json"
   ```

   Or edit `~/.claude.json` (or `.claude/settings.json` at project scope)
   and add:

   ```json
   {
     "mcpServers": {
       "llmlens": {
         "command": "/absolute/path/to/llmlens/bin/llmlens",
         "args": [
           "serve",
           "--protocol=mcp",
           "--profile=/absolute/path/to/llmlens/profiles/linkedin.json"
         ]
       }
     }
   }
   ```

3. **Start a new Claude Code conversation** in this directory. The MCP
   server starts on demand; Chrome launches when the first tool call fires
   (currently eager — see notes).

**Run.** In the new conversation:

> Use the llmlens tools to find my 1st and 2nd order LinkedIn connections
> who work in AI/ML/LLMs. For each connection: name, headline, current
> company, profile URL. Cap at ~50 1st-order and ~50 2nd-order. Save
> results to `runs/<id>/connections.csv`. Pace navigations to avoid
> LinkedIn rate-limiting; if you hit a "Search limit reached" page, stop
> and report what you have.

**Pass criteria.**
1. Agent successfully navigates LinkedIn search filtered to 1st-order
   network with AI-related keywords.
2. Snapshots expose result-card elements (name, headline) cleanly. If the
   AXTree is too sparse, the agent falls back to `screenshot` for visual
   inspection.
3. Agent paginates with reasonable delays (≥2s between page transitions).
4. Final CSV exists and contains plausible AI-relevant connections (not
   just every single connection regardless of headline).

**Failure modes likely to surface.**
- **Iframe gaps**: parts of LinkedIn's feed/search may live in iframes our
  AXTree capture doesn't traverse. This is the trigger for Phase 2
  perception work (cross-frame snapshot).
- **Rate-limiting**: LinkedIn shows a "You've reached the search limit"
  interstitial. Agent should detect via snapshot text and stop gracefully
  rather than retrying forever.
- **Stale refs**: LinkedIn's lazy-rendered cards mutate; refs from one
  snapshot may be invalid in the next. Error code `stale` surfaces this
  cleanly so the agent can re-snapshot.
- **Identity-verification challenge**: triggers PRD §3.3 — would justify
  the extension path.

**Notes from the first agent run** (run-id `20260430T075521Z-bedeb2da`):
- The agent reached for `eval` for everything — 28 evals, 0 snapshots. It
  worked but bypassed our perception layer. Root cause: snapshot's element
  output didn't expose `href`, so even with names visible, profile URLs
  required eval.
- Fixed by adding `Href` to `Element` for `role=link` nodes. A subsequent
  run should prefer snapshot over eval. If you re-run this scenario and
  still see ≥10 evals + 0 snapshots, the snapshot tool description in
  `internal/rpc/mcp.go::toolDescriptors()` may need to be sharper.

**Notes.**
- The MCP server currently launches Chrome eagerly on `serve` start. For a
  smoother demo where the browser only spins up on first use, lazy launch
  is a small future change.
- Run artifacts (per-page HTML + markdown + the JSONL events log) land
  under `runs/<run-id>/`. Inspect after the run to debug agent behaviour.

## Conventions for adding scenarios

- A scenario is one binary subcommand (`-scenario=<name>`).
- Pass criteria must be machine-checkable; soft assertions belong in logs, not
  the gate.
- Scenarios that need real auth or external state are documented as
  **manual** and not part of any default CI loop.
- All scenarios persist artifacts to `runs/<run-id>/`; failure messages
  should print this path so the operator can inspect.
