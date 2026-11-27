# 0099. README GIFs and recording

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-06, SC-07, SC-08, CP-15

## Context

SC-06 asks for a README with an architecture diagram, one GIF per scenario, the benchmark, accuracy and chaos tables and a quick start of three commands or fewer; the M8 box names vhs for terminal scenes and a screen recorder for the UI. SC-07 asks for a 3–4 minute walkthrough video (D1, D2, D4, D5), which the user records, and a live-demo script in `docs/demo.md`. The run's rules: every tool pinned exactly (images by digest); GIFs and recordings show no key value, token, JWT or password; recording artifacts are git-ignored and removed after every run.

What exists: Playwright 1.64.1 pinned in `web/package.json` with Chromium installed by `make test-e2e`; the managed key grammar `spw_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}` (`internal/apikey.Pattern`); the vhs image `ghcr.io/charmbracelet/vhs:v0.12.1` (multi-arch, built 2026-09-24) whose layers install `ffmpeg`, `chromium` and `bash` (its build history shows `apt-get -y install ffmpeg chromium bash`); no `curl` in it.

## Decision

### 1. Tools (D26)

- **vhs** as the image `ghcr.io/charmbracelet/vhs:v0.12.1@sha256:ea49a6a1c529be83153e88321892b5585964418f0b9055e8c1e0d732194234a3`, run with `docker run` (no install on the host). The same image's `ffmpeg` (`--entrypoint ffmpeg`) converts the Playwright videos, so one pinned image covers both.
- **UI recorder:** Playwright's video recording (1.64.1, the repository's pin), a separate Playwright project `media` (specs in `web/e2e/media/`) that `make test-e2e` and CI never run.
- No new npm or Go dependency.

### 2. Scenes

| GIF (`docs/media/`) | Recorder | Shows |
| --- | --- | --- |
| `d1-fair-limits.gif` | vhs | `demo-traffic probe` for `free-co-mobile` then `pro-co-mobile` on `orders`: the free key reaches 429 with `Retry-After`, `RateLimit` and `RateLimit-Policy` (`q=10`), pro keeps getting 200 (`q=100`) |
| `d2-noisy-neighbour.gif` | Playwright | the Playground running scenario D2: `free-co` mostly denied at 50× its limit, `pro-co` and `enterprise-co` allowed with flat p99 |
| `d3-distributed-accuracy.gif` | vhs | `make demo-d3`'s per-second admitted rate on the 100/s line over three replicas and its accuracy summary |
| `d4-safe-rollout.gif` | Playwright | Policies: a stricter policy in shadow, its shadow report naming `enterprise-co-web`, promote to enforce, roll back from History |
| `d5-redis-failure.gif` | vhs | `make demo-d5`: `catalog` keeps serving (fail-open), `payments` answers 503 (fail-closed), `orders` holds about 33/s per replica (degraded), then recovers |
| `d6-upstream-protection.gif` | vhs | `make demo-d6`: circuits open on all replicas, retries capped by the budget (`budget_exhausted` grows), circuits close after the fault |
| `d7-leaked-key.gif` | Playwright | Throttling: the flooding key at the top of heavy hitters, Block key with a note, its requests answered 403 |

D5's GIF is a terminal scene so the video (which covers D5 in the UI) and the README do not show the same thing twice.

### 3. How each is recorded

- **`make demo-gifs`** (script `deploy/demo/media.sh`) needs a running demo (`getDemo` 200) and records every scene, or one with `SCENE=d4`:
  - **Terminal scenes:** builds a static `demo-traffic` for the Docker host's architecture (`CGO_ENABLED=0 GOOS=linux go build -o bin/media/demo-traffic ./cmd/demo-traffic`; `bin/` is ignored), then runs the vhs image with `--network spillway_default`, the tapes `docs/media/tapes/*.tape` and the binary mounted read-only, `docs/media/.work/` as its output, and the `SPILLWAY_DEMO_*` settings passed by name (`-e NAME`, values from compose's dev defaults, never on a command line). The container gets no Docker socket: it runs `demo-traffic` against the demo stack only. Scenario scenes stop the base-load service and start it again, as `make demo-dN` does, from the script on the host.
  - **Tape settings:** `Set Width 960`, `Set Height 540`, `Set FontSize 16`, `Set Framerate 12`, `Set TypingSpeed 40ms`, `Set Theme "Builtin Dark"`, `Set PlaybackSpeed` so the GIF is at most 20 s (3.0 for the 60 s scenarios), and two outputs: the GIF and a `.ascii` text capture of the same session for the leak check.
  - **UI scenes:** `pnpm exec playwright test --project media` against the demo stack (UI on `http://localhost:8080`, the demo Grafana through `http://localhost:8888/grafana`), viewport and video size 960×540, one worker, no retries. Each spec logs in as `test-admin` (D4, D7) or `test-viewer` (D2) with the realm's dev password, then records the scene start (milliseconds since the context started) in `web/test-results/<scene>.json`; the conversion cuts everything before it, so the login form never appears. D2's spec presses the Playground's D2 button; D4's drives the Policies and History screens with the base load running; for D7 the script starts `make demo-d7` (no `BLOCK`) in the background and the spec waits for the key on Throttling, then blocks it.
  - **Conversion:** `ffmpeg -ss <start> -t 20 -i <scene>.webm -vf "fps=12,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128[p];[b][p]paletteuse=dither=bayer" -loop 0 docs/media/<scene>.gif` in the vhs image.
- **Size:** each GIF at most **2 MB** and 20 s, 960 px wide, 12 fps; all seven at most 14 MB. Over 2 MB, the script retries that GIF at 64 colours, then at 10 fps, and fails if it is still over. Each GIF's size is printed.
- **Regenerate:** `make demo` then `make demo-gifs` (about 10 minutes); the GIFs are committed by hand in the PR that changes them. `docs/demo.md` describes it.

### 4. No keys, tokens or passwords in any frame

- **What is never on screen.** No scene opens the issue-key or rotate dialogs. Terminal output names keys by ID (`key #412`, ADR-0096 §1); the Playground never receives a key (UI-07); Throttling shows a key's ID and public prefix only (CP-15's prefix is not the key). No scene prints the environment.
- **Checks that fail `make demo-gifs` and delete the GIF:**
  - terminal: each tape's `.ascii` capture must not match the key grammar `spw_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}`, a JWT (`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.`), `dev-only-not-secret` or `client_secret`;
  - UI: at every step each spec checks `document.body.innerText` and every input's value for the same patterns; a console listener fails the spec on any console message; a response listener fails it when a response body matches the key grammar, except Keycloak's token endpoint (which returns tokens to the page by design, never rendered).
- The login form is cut from every video (§3), and its password field is masked anyway.

### 5. Artifacts and clean-up

- Committed: `docs/media/*.gif`, `docs/media/tapes/*.tape`, `web/e2e/media/*.spec.ts`, `deploy/demo/media.sh`.
- Git-ignored and removed by the script on every exit: `docs/media/.work/` (`.ascii` captures, intermediate files), `bin/media/`, `web/test-results/`, `web/playwright-report/`. The readme task adds `docs/media/.work/` to `.gitignore` (`bin/`, `web/test-results/` and `web/playwright-report/` are ignored already).

### 6. README and `docs/demo.md`

- **readme task:** the README gets the seven GIFs next to their scenario rows, the existing benchmark, accuracy and chaos tables, the architecture diagram, and a quick start of at most three commands: `git clone https://github.com/ankitbisht23/spillway.git && cd spillway`, `make demo`, `make demo-d1` (with a pointer to the UI at `http://localhost:8080`, login `test-viewer`). It applies P53 and the `docs/` part of P54.
- **demo-doc task:** `docs/demo.md` is the live-demo script: prerequisites, `make demo`, for each of D1–D7 the command or clicks, what the viewer should see and the numbers it proves, how to recover (`make demo-fault-clear`, `make demo-reset`), `make demo-share` for a public link (ADR-0097), and a shot list for the user's 3–4 minute video (D1, D2, D4, D5) with timings. It updates setup.md Step 6's "Record" item to point at `make demo-gifs` and the shot list. The video itself and SC-07's box stay with the user.

## Consequences

- The GIFs are reproducible from the repository with two pinned tools, and a leaked key or token in any frame fails the recording before it can be committed.
- `make demo-gifs` needs Docker, the demo running and Chromium (as `make test-e2e`); it is never part of CI.
- Seven GIFs add at most 14 MB to the repository; regenerating them replaces those files.

## Alternatives considered

- **vhs installed on the host** (`go install` plus ttyd and ffmpeg) — three host tools to pin per platform; the image brings all of them at one digest.
- **OBS Studio or QuickTime for UI scenes** (setup.md's suggestion for the video) — manual, not reproducible, and nothing checks the frames; they stay fine for the user's video.
- **A separate ffmpeg image** — one more digest when the vhs image already carries ffmpeg.
- **MP4 or WebM in the README** — GitHub renders GIFs inline everywhere; video embeds need uploads, which the run does not do.
