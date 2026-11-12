# Spillway — Prerequisites & Setup Guide

Nov 2, 2026 · @Ankit Bisht

## Who does what

You handle anything that needs a login, a browser console, card or phone verification, admin rights on your machine, or a decision. The agent (Claude Code here; any terminal agent works) builds everything else, including Postgres, Redis Cluster, Keycloak and the observability stack, all in Docker.

Only the first seven rows block M0 (two are already done); the rest can wait until the milestone named.

| Your task | Why the agent can’t | Needed by |
| --- | --- | --- |
| Prepare the machine; install a Docker runtime | Admin rights, GUI installer, licence acceptance | M0 |
| Give Docker ≥ 8 GB memory (10 GB once kind runs in M7) | GUI or VM setting | M0 |
| Install the toolchain (Go, Node, pnpm, git, gh, make) | sudo and shell-profile changes | M0 |
| Create the GitHub repo; add an SSH key | Your account, password and 2FA | M0 |
| Log `gh` in to the personal account; keep work credentials out of this repo | Needs your browser login | M0 |
| Install and log in to Claude Code; export the design doc into the repo (**done**: `docs/design.md`, `docs/setup.md`) | Your Claude login; the agent can’t read claude.ai docs | M0 |
| Decide project name, licence, Go module path, public or private (**done**: `spillway`, MIT, public; ADR-0001) | Your call | M0 |
| Add GitHub Actions secrets | Secret values come from you | M8 (optional) |
| Run live demos; record the walkthrough video | Your voice and judgement | M8 |
| Optional: GitHub OAuth app for demo login | Browser console; issues a client secret | M8 |
| Optional: Cloudflare domain, Grafana Cloud | Signups, payment for the domain | M8 |
| Optional: AWS or Google Cloud account for an always-on demo | Card check, web console | M8 |
| Approve each milestone’s exit check | Judgement | Every milestone |

**The agent handles:** monorepo scaffold, `compose.yaml` (Redis Cluster, Postgres, Keycloak, Prometheus, Grafana, Tempo, Loki, OTel Collector), migrations and seed data, Keycloak realm import, Makefile, CI workflows, Helm chart and kind cluster config, benchmark workflow, k6 and chaos scripts, demo seed data and traffic simulator, all code and tests.

Build this on a personal machine and personal GitHub account, not work devices or accounts, so the code and repo stay unambiguously yours. If this machine also has a work GitHub account or work kube contexts, follow the isolation steps in Step 3 (they build on the per-folder git identity in Step 2; the kind cluster gets its own kubeconfig in Step 6).

## Requirements at a glance

To start M0 you need a 16 GB machine, Docker, a GitHub account and a paid Claude plan; everything else is free and can wait.

**Machine**

- [ ] macOS 13+, Ubuntu 22.04+, or Windows 10/11 with WSL2 (Ubuntu)
- [ ] 16 GB RAM minimum (8–10 GB of it for Docker), 32 GB recommended; 4+ CPU cores; 40 GB free disk

**Accounts**

- [ ] GitHub, personal account (free)
- [ ] Claude Pro or Max, or an Anthropic Console account; the free Claude plan doesn’t include Claude Code ([source](https://code.claude.com/docs/en/setup))
- [ ] Optional, M8: Cloudflare domain and Grafana Cloud (free tiers) for a permanent demo
- [ ] Optional, M8: AWS or Google Cloud credits for an always-on demo (card required)

**Software**

- [ ] Docker runtime: Docker Desktop (macOS, Windows) or Docker Engine (Linux)
- [ ] Go, Node LTS, pnpm, git, gh, make, jq, mkcert
- [ ] kubectl, kind, helm, k6, cloudflared, vhs (needed by M7–M8; cheap to install now)
- [ ] Claude Code
- [ ] A password manager for secrets
- [ ] Optional: an editor (VS Code with the Go extension) for reviewing code; OBS Studio for recording

## Step 1 — Prepare your machine and Docker

The whole stack runs in Docker locally, so the one thing that must be right is a Docker runtime with ≥ 8 GB memory that runs without `sudo`. Docker Desktop is free for personal use ([licence](https://docs.docker.com/subscription/desktop-license/)).

### macOS

1. Update to macOS 13 or later; run `xcode-select --install` for git and make.
2. Install Homebrew:

   ```bash
   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
   ```
3. Install Docker Desktop: `brew install --cask docker`, open it once and accept the terms.
4. Docker Desktop → Settings → Resources: Memory 8 GB (10 GB if you have 32 GB RAM), CPUs 4+, disk 64 GB. Apply and restart.
5. Raise the open-files limit (load tests open thousands of sockets):

   ```bash
   sudo launchctl limit maxfiles 65536 200000   # resets on reboot
   echo 'ulimit -n 65536' >> ~/.zshrc
   ```
6. Turn off AirPlay Receiver: System Settings → General → AirDrop & Handoff → AirPlay Receiver. It listens on ports 5000 and 7000, and Redis Cluster needs 7000–7005. Verify:

   ```bash
   lsof -nP -iTCP:7000 -sTCP:LISTEN   # prints nothing
   ```

### Windows (WSL2)

1. In an admin PowerShell: `wsl --install -d Ubuntu-24.04`, reboot, create your Linux user.
2. Cap WSL memory so Windows stays usable: create `%UserProfile%\.wslconfig`, then run `wsl --shutdown`.

   ```ini
   [wsl2]
   memory=10GB
   processors=4
   ```
3. Install Docker Desktop for Windows; in Settings enable “Use the WSL 2 based engine” and Resources → WSL integration for Ubuntu.
4. Do everything else, including Claude Code, inside the Ubuntu terminal. Keep the repo in the Linux filesystem (anywhere under your home directory), never under `/mnt/c`: builds and bind mounts are many times faster on the Linux filesystem.
5. Raise open files: `echo 'ulimit -n 65536' >> ~/.bashrc`.

### Ubuntu (native)

1. Install Docker Engine and the Compose plugin with Docker’s convenience script (or the apt steps on docs.docker.com):

   ```bash
   curl -fsSL https://get.docker.com | sh
   sudo usermod -aG docker $USER   # log out and back in afterwards
   ```
2. Raise open files: `echo 'ulimit -n 65536' >> ~/.bashrc`.

### Verify (all platforms)

```bash
docker run --rm hello-world          # prints Hello from Docker!
docker compose version               # 2.24 or later (optional env files, Step 5)
docker info --format '{{.MemTotal}}' # bytes; want ≥ 8000000000
```

The open-files limit you raised covers host processes only (anything you run directly on the host, such as k6 or `cmd/loadgen` while exploring). Containers don’t inherit it; they get theirs from `ulimits` in `compose.yaml` (Step 5).

The agent cannot do this step: it needs admin rights, a GUI installer and licence acceptance, and the agent must be able to call `docker` without `sudo`.

## Step 2 — Install the toolchain

Use Homebrew on every platform (it runs on Linux and WSL2 too), so one command installs everything global; the repo pins the rest.

1. Linux and WSL2 only: install Homebrew’s prerequisites, Homebrew, and put it on your PATH.

   ```bash
   sudo apt update && sudo apt install -y build-essential procps curl file git libnss3-tools
   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
   echo 'eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv)"' >> ~/.bashrc && source ~/.bashrc
   ```
2. Install the tools (macOS, Linux, WSL2):

   ```bash
   brew install go fnm pnpm gh jq mkcert kubectl kind helm k6 cloudflared vhs
   ```

   Project tools don’t come from brew: `make tools` (added by the agent in M0) installs pinned sqlc, goose, golangci-lint, buf, gitleaks, oasdiff, govulncheck, protoc-gen-go, protoc-gen-go-grpc and oapi-codegen into `./bin` (gitignored). The repo also pins Go, Node and pnpm: the `toolchain` line in `go.mod` (a minimum: Go downloads it if yours is older; CI and the Makefile set `GOTOOLCHAIN` to the exact version), `.node-version` (fnm’s `--use-on-cd` switches to it when you enter the repo; `fnm install` fetches it if missing) and `packageManager` in `package.json`.
3. Install Node LTS through fnm and load it in every shell. Every shell-profile line in this guide goes in `~/.zshrc` on macOS and `~/.bashrc` on Linux and WSL2; run the line for your platform:

   ```bash
   echo 'eval "$(fnm env --use-on-cd)"' >> ~/.zshrc && source ~/.zshrc     # macOS
   echo 'eval "$(fnm env --use-on-cd)"' >> ~/.bashrc && source ~/.bashrc   # Linux, WSL2
   fnm install --lts && fnm default lts-latest
   ```
4. Put Go-installed binaries on your PATH:

   ```bash
   echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.zshrc && source ~/.zshrc     # macOS
   echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.bashrc && source ~/.bashrc   # Linux, WSL2
   ```
5. Trust a local certificate authority for HTTPS on localhost (asks for your password once). `make certs` mints the gateway's certificate with the pinned mkcert in `./bin` either way; this step makes browsers and curl trust it:

   ```bash
   mkcert -install
   ```
6. Configure git with your GitHub no-reply address (Step 3 shows where to find it).

   Personal machine, no work identity: set it globally.

   ```bash
   git config --global user.name '<Your Name>'
   git config --global user.email '<id>+ankitbisht23@users.noreply.github.com'
   git config --global init.defaultBranch main
   ```

   Machine with a work git identity: leave the work identity global and give the repo folder its own identity and SSH key (the key is created in Step 3). Run this from inside the repo folder:

   ```bash
   git config --file ~/.gitconfig-spillway user.name '<Your Name>'
   git config --file ~/.gitconfig-spillway user.email '<id>+ankitbisht23@users.noreply.github.com'
   git config --file ~/.gitconfig-spillway core.sshCommand 'ssh -i ~/.ssh/id_ed25519_spillway -o IdentitiesOnly=yes'
   git config --global "includeIf.gitdir:$PWD/.path" ~/.gitconfig-spillway
   git config --global init.defaultBranch main
   ```

   This adds `[includeIf "gitdir:<repo path>/"]` to `~/.gitconfig`; keep it below the global `[user]` section (`git config` appends it there) so it wins inside the repo.
7. Verify:

   ```bash
   go version && node -v && pnpm -v && gh --version && jq --version
   kubectl version --client && kind version && helm version && k6 version && cloudflared --version
   git config user.email   # inside the repo: prints the no-reply address
   ```

The agent cannot do this step reliably: it needs `sudo`, a password prompt for the trust store, and edits to your shell profile.

## Step 3 — GitHub repo, SSH and CI

Make the repo public: standard GitHub-hosted runners are free in public repositories, while private repos on GitHub Free get 2,000 minutes a month ([billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions)). Nightly load tests and multi-arch image builds would eat a private quota fast.

The local repo already exists with no commits yet; this step secures the account, gives the repo a personal identity and pushes it. Any folder works for the repo as long as iCloud doesn’t sync it (it syncs Desktop and Documents when iCloud Drive’s “Desktop & Documents Folders” option is on), and the folder name needn’t match the repo name. On WSL2, keep it inside the Linux filesystem.

1. **Secure the account.** Settings → Password and authentication → enable two-factor authentication.
2. **Hide your email.** Settings → Emails → tick “Keep my email addresses private” and copy the `…@users.noreply.github.com` address into the git config from Step 2.
3. **SSH key.** If `~/.ssh/id_ed25519` doesn’t exist, create the key at the default path. If it exists and is already your personal key, reuse it. If it belongs to another account (for example a work key), never overwrite it: create a dedicated key and use the per-folder identity from Step 2, whose `~/.gitconfig-spillway` points git at it.

   ```bash
   ssh-keygen -t ed25519 -C '<no-reply address>'                                 # no existing key: default path
   ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_spillway -C '<no-reply address>'   # a key exists: dedicated path
   ```

   Set a passphrase, then load the key into ssh-agent so pushes from the agent’s shell don’t stop at a passphrase prompt: `ssh-add --apple-use-keychain <key>` on macOS, `ssh-add <key>` on Linux. Add the public key (`cat <key>.pub`) to the personal account: Settings → SSH and GPG keys → New SSH key. Verify from inside the repo; this uses the repo’s `core.sshCommand` if set, plain `ssh` otherwise:

   ```bash
   eval "$(git config core.sshCommand || echo ssh) -T git@github.com"   # expect: Hi ankitbisht23! You've successfully authenticated
   ```
4. **Log `gh` in to the personal account** in its own config directory, so a work `gh` login (if any) stays untouched:

   ```bash
   GH_CONFIG_DIR=~/.config/gh-personal gh auth login    # GitHub.com → SSH → skip the key upload (done above) → log in with browser
   GH_CONFIG_DIR=~/.config/gh-personal gh auth status   # expect: Logged in to github.com account ankitbisht23
   ```

   In your own terminal, `export GH_CONFIG_DIR=~/.config/gh-personal` whenever you work in this repo, or add `alias ghp='GH_CONFIG_DIR=~/.config/gh-personal gh'` to your shell profile (`~/.zshrc` on macOS, `~/.bashrc` on Linux and WSL2). Agent sessions get it from `.claude/settings.local.json` (Step 9).
5. **Name, licence and module path: decided** (ADR-0001).

   | Decision | Value |
   | --- | --- |
   | Name | `spillway` |
   | Licence | MIT |
   | Repo | `github.com/ankitbisht23/spillway`, public |
   | Go module | `github.com/ankitbisht23/spillway` |

   The module path is the GitHub path, not a vanity import path: a vanity path needs a domain you keep serving forever, and changing a module path later breaks every import.
6. **Bootstrap commit and create the GitHub repo from the local folder.** Review first: `git status --short` must list no `.env*` files, keys or `.claude/settings.local.json`, and `git config user.email` must print the no-reply address.

   ```bash
   git add -A && git commit -m "Bootstrap: design, setup, agent config, ADRs"
   GH_CONFIG_DIR=~/.config/gh-personal gh repo create ankitbisht23/spillway --public --source=. --remote=origin --push
   ```

   This is the one direct commit to `main`; the ruleset comes next, and everything after it is a PR.
7. **Protect `main`.** Settings → Rules → Rulesets → New branch ruleset targeting `main`, enforcement Active, bypass list empty. Turn on: require a pull request, require status checks, block force pushes, restrict deletions. The only required check is `ci-ok`, the single aggregate CI job that passes only when every other CI job does; GitHub lists it once it has run, so add it after M0’s first workflow run. Solo repo: don’t require approvals, since you can’t approve your own PR. The agent still can’t merge without asking: `gh pr merge` is an ask rule (Step 9).
8. **Turn on free security features.** Settings → Code security: Dependabot alerts, secret scanning and push protection. Dependabot version updates come from `.github/dependabot.yml`, which the agent adds in M0.
9. **After the first image push (the v1.0.0 release in M7, or earlier if CI starts publishing images):** open your profile → Packages → `spillway-gateway` and `spillway-controlplane` → Package settings → Change visibility → Public, so anyone can pull `ghcr.io/ankitbisht23/spillway-gateway` and `ghcr.io/ankitbisht23/spillway-controlplane` without credentials.
10. **Note:** the bench workflow creates and pushes the unprotected `bench-results` branch itself (ADR-0003); keep it out of the ruleset.

The agent can create branches, commits, PRs and workflow files through `gh` once you’ve logged in; the SSH key, `gh` login, bootstrap push, account security, rulesets and package visibility need you.

## Step 4 — Auth providers

Local development needs no signup: the agent runs Keycloak in Docker as the single OIDC provider for admin login and the JWT issuer for gateway tests. You register a GitHub OAuth app only for the public demo, and Keycloak brokers it, so the app code stays pure OIDC.

### Local (agent builds it, you check it)

The agent commits a realm import (`deploy/compose/keycloak/realm.json`) containing:

- realm `spillway`, with roles `admin`, `operator`, `viewer` (CP-12)
- client `admin-ui`: public, authorization code + PKCE, redirect URIs for the Vite dev server and the embedded UI on the control plane (ports in `docs/ports.md`); the demo hostname is added in M8
- client `gateway-tests`: confidential, client-credentials, for minting test JWTs (GW-18)
- one test user per role, with obviously fake dev-only passwords (Step 8)

People log in through Keycloak only, and service tokens come from Keycloak confidential clients using client credentials, like `gateway-tests`; the control plane stores no passwords (CP-11).

In dev, Keycloak runs with `KC_HOSTNAME=http://localhost:8180/auth`, `KC_HTTP_RELATIVE_PATH=/auth` (the same path the demo proxy uses) and `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`. Tokens carry the issuer your browser sees (`http://localhost:8180/auth/realms/spillway`), and services check `iss` against it, but they fetch the signing keys (JWKS) from a separately configured URL inside the compose network (`http://keycloak:8080/auth/realms/spillway/...`, Keycloak’s container port, not the control plane’s host port). Plain-HTTP issuers are accepted only with `dev_mode: true`, never in Helm (SEC-04).

Your part: after `make dev`, open the Keycloak admin console at `http://localhost:8180/auth/admin/` (port from `docs/ports.md`; Keycloak runs under `/auth`) and log in once to confirm it works.

### Demo login with GitHub (optional, M8)

1. GitHub → Settings → Developer settings → OAuth Apps → New OAuth App.
2. Fill in:
   - Application name: `spillway admin (demo)`
   - Homepage URL: `https://<demo-host>`
   - Authorization callback URL: `https://<demo-host>/auth/realms/spillway/broker/github/endpoint` (Keycloak sits at `/auth` behind the demo proxy, Step 6)

   `<demo-host>` is the demo proxy’s hostname, `admin.<domain>` in Step 7. The callback must stay fixed, so GitHub login needs that named tunnel; a quick tunnel’s URL changes every run, so use the realm’s test users with it.
3. Register, then Generate a new client secret. Copy the client ID and secret into your password manager immediately; the secret is shown once.
4. Optional: register a second app for local testing with callback `http://localhost:<keycloak-port>/auth/realms/spillway/broker/github/endpoint` (Keycloak’s port is in `docs/ports.md`), since each OAuth app takes one callback URL.
5. Give the values to the deployment as `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET` (Step 8). The agent wires Keycloak’s GitHub identity provider to read them.
6. After your first GitHub login on the demo, grant your user the `admin` role in Keycloak; leave visitors on `viewer`.

The agent cannot register OAuth apps: it takes your GitHub session in a browser and produces a secret only you should hold.

## Step 5 — Local databases and observability

Don’t install Postgres, Redis, Keycloak or any monitoring tool on your machine: the agent defines all of them in `compose.yaml` at the repo root, with service configs under `deploy/compose/<service>/`. Your job is to free the ports, keep Docker’s memory allocation, and know how to reset.

Everything runs in compose by default, Spillway’s own processes included: the gateway, the control plane, the load generator and accuracy harness, and the demo traffic. `make dev` is `docker compose up -d --wait`, so it returns once every service reports healthy. Multi-replica runs add `gateway-2` … `gateway-5` (compose profile `replicas`); only `gateway-1` publishes host ports. The load generator runs on demand, `docker compose run --rm --build loadgen -url http://gateway-1:8000/bytes/1024 -rate 1000` (compose profile `tools`, ADR-0014), and prints its result as JSON. Redis Cluster nodes announce hostnames (`redis-N`) that resolve only inside the compose network, so a Go process or test you run on the host reaches them through an address map (`redis-N:6379` → `127.0.0.1:700N`), turned on by `SPILLWAY_REDIS_ADDR_MAP` in dev and tests only. Details: ADR-0004 (`docs/adr/0004-dev-networking.md`). The gateways find the cluster through `SPILLWAY_GATEWAY_REDIS_ADDRS`, which compose sets to the six nodes; a config with a Redis policy (one without `local: true`) is refused without it (ADR-0021). The accuracy harness runs as the compose service `accuracy` (profile `harness`): `make accuracy REPLICAS=3` starts gateway-2 … gateway-N, swaps in `test/accuracy/gateway.yaml` and prints its JSON report; it reads the `Spillway-Decision-Time` header that compose's gateways send because they run with `SPILLWAY_GATEWAY_DEV_MODE=true`, which production must never set (ADR-0021). A gateway shuts down gracefully (GW-23, ADR-0030): on SIGTERM its `/readyz` fails at once, it keeps serving for `SPILLWAY_GATEWAY_DRAIN` (5 s), then closes its listeners and gives in-flight requests `SPILLWAY_GATEWAY_SHUTDOWN_GRACE` (15 s), so `docker compose stop` or a restart of a gateway takes at least 5 s; set both in `.env.local` to change them (`SPILLWAY_GATEWAY_DRAIN=0s` skips the drain). Each gateway also caps its open client connections (`SPILLWAY_GATEWAY_MAX_CONNECTIONS`, 8192) and the requests it handles at once (`SPILLWAY_GATEWAY_MAX_IN_FLIGHT`, 4096); past either it answers 503 with `Retry-After: 1` and `{"error":"overloaded"}` at once, counted as `gw_requests_total{reason="shed"}`, instead of queueing (GW-26, ADR-0030). A connection past the cap gets that answer as soon as its request starts to arrive, without the rest being read (after a TLS handshake offering only HTTP/1.1 on the TLS port), or a reset when 256 such connections are already being answered. The admin port is exempt. Each gateway sends a trace of every request to Tempo through the OTel Collector, with a span per stage and one per upstream attempt (OB-02, ADR-0030); the Collector keeps errors, requests slower than 500 ms and a tenth of the rest, and on the dev config `GET /retry/status/503` shows a request tried three times. The checks that measure latency run the gateways at the benchmarks' fixed 1% head ratio (`SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO=0.01`). The Redis chaos tests run from the host with the docker CLI and always bring the cluster back: `make chaos-redis-outage` pauses all six nodes while loadgen drives gateway-1 … gateway-3, and `make chaos-redis-primary` kills the primary that owns slot 0 while the compose service `chaos` (profile `harness`) drives gateway-1. The second needs `cluster-require-full-coverage no` from `deploy/compose/redis/redis.conf`, which Redis nodes started before it was added lack until `docker compose down -v` (ADR-0021).

Ports live in `docs/ports.md`, the source of truth; `compose.yaml` and the Helm values must match it. In short: Postgres, six Redis Cluster nodes, Keycloak, the gateway (TLS traffic, plain HTTP for dev and benchmarks, admin), the control plane (REST, admin, gRPC), the Vite dev server, the observability stack, three mock upstreams, toxiproxy, the demo proxy (M8) and kind’s 443 (M7), all on localhost.

1. **Free the ports.** Run the free-port check in `docs/ports.md`; it should print nothing (on macOS, AirPlay Receiver must be off, Step 1). Stop whatever it finds, one service per command, for example a Homebrew Postgres or nginx (`brew services stop` also keeps it from starting at login):

   ```bash
   brew services stop postgresql@16   # if installed via brew
   brew services stop nginx           # if it holds 8080
   ```

   A Homebrew Redis on its default port 6379 doesn’t clash and can keep running.
2. **Keep Docker at ≥ 8 GB.** The full stack plus a load test is roughly 4–6 GB (estimate); below 8 GB, Redis Cluster nodes and Keycloak get OOM-killed.
3. **Container open-files limit.** The host `ulimit -n` from Step 1 doesn’t reach containers; `compose.yaml` sets `ulimits: { nofile: 65536 }` for the gateway and load generators.
4. **Env files.** The Makefile passes `--env-file .env.example` to compose and adds `--env-file .env.local` only if that file exists; services list `.env.local` as an `env_file` with `required: false`. A clean clone boots with the committed defaults, and `.env.local` (Step 8) overrides only what you put in it.
5. **Know the reset.** `docker compose down -v` deletes every container and volume; `make dev` rebuilds from migrations and seed data. Postgres gets a named volume. Redis keeps nothing: no persistence, `nodes.conf` on tmpfs, and a one-shot `redis-init` service creates the cluster fresh on every `up`.
6. **Nothing to sign up for.** All images are open source and pulled from Docker Hub or Quay. Anonymous Docker Hub pulls are rate-limited; if `make dev` hits that limit, run `docker login` with a free Docker account.

The agent can build and run this entire section; the only human parts are stopping conflicting local services and the optional `docker login`.

## Step 6 — Kubernetes, benchmarks and demo without a cloud VM (M1, M7–M8)

No cloud account is needed. Kubernetes runs as a local kind cluster, benchmarks run on free GitHub Actions runners, and live demos run from your laptop through a Cloudflare quick tunnel. An always-on hosted demo is optional and runs on time-limited cloud credits.

| Job | Where it runs | Card needed | Limitation |
| --- | --- | --- | --- |
| Kubernetes and Helm (M7) | Local 3-node kind cluster | No | Not reachable from the internet |
| Benchmarks (M1), nightly load tests (M7) | GitHub Actions on your public repo: 4 vCPU, 16 GB, free and unlimited ([runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)) | No | Shared hardware; gateway and load generator share 4 vCPUs |
| Live demo (M8) | Your laptop + Cloudflare quick tunnel | No | URL changes every run; laptop must stay on |
| Always-on demo (optional) | AWS or Google Cloud credits | Yes | Credits expire |

### Kubernetes on kind (you: almost nothing)

1. Raise Docker’s memory to 10 GB if your machine allows it; three kind nodes plus the stack need the headroom. On a 16 GB machine, close heavy apps while the cluster runs.
2. The agent writes `deploy/kind/cluster.yaml` (1 control-plane + 2 workers) and `make kind-up` / `make kind-down`. There is no ingress controller: the gateway is the edge, a NodePort Service that kind’s `extraPortMappings` expose on `127.0.0.1:443` only (`listenAddress: 127.0.0.1`, so nothing is reachable from the local network; no port 80, because the gateway serves no plain HTTP in Helm). The cluster is `spillway`, its context `kind-spillway`, and it lives in its own kubeconfig, `~/.kube/spillway-kind`, not `~/.kube/config`. The Makefile sets `export KUBECONFIG := $(HOME)/.kube/spillway-kind` (`:=`, so a `KUBECONFIG` inherited from your shell can’t redirect kind into another file), passes `--kubeconfig` to kind, and always passes `--context kind-spillway`, so its commands can never reach another cluster, including any work contexts on this machine.
3. In your own terminal, `export KUBECONFIG=~/.kube/spillway-kind` when you work on the cluster and pass `--context kind-spillway` to every kubectl and helm command (`--kube-context` for helm). Agent sessions get the kubeconfig from `.claude/settings.local.json` (Step 9).
4. Verify once: `make kind-up && kubectl --context kind-spillway get nodes` shows three Ready nodes; `make kind-down` removes them.

### Benchmarks on GitHub Actions (you: nothing beyond Step 3)

1. M1: the agent adds `.github/workflows/bench.yml` with a manual trigger only, on `ubuntu-latest` (x64) and `ubuntu-24.04-arm` (arm64), and records the first baseline. M7 adds the nightly schedule and the regression gate.
2. CPU layout on the 4-vCPU runner: gateway pinned to 1 vCPU, load generator 2, Redis and mock upstream 1. The scaling check (NFR-02, M7) runs the gateway on 1 and then 2 vCPUs, with the load generator and upstream on 1 each and no Redis, and counts only if the load generator reached its target rate. Each run pushes its results as JSON to the unprotected `bench-results` branch under `results/<arch>/<UTC-timestamp>.json`; the gate compares a run with the latest baseline of the same arch, and README tables are updated by PR (ADR-0003).
3. Run the first one yourself in M1 to see it work: `gh workflow run bench.yml && gh run watch`.
4. Once the schedule lands (M7): scheduled workflows in public repos pause after 60 days without repository activity; re-enable with `gh workflow enable bench.yml`. Runner hardware is shared, so the design’s benchmark method uses 5 runs and reports the median.

### Live demo from your laptop (you)

1. Start the stack: `make demo` (built in M8).
2. Share it:

   ```bash
   make demo-share   # quick tunnel to the demo proxy on 8888; prints a trycloudflare.com URL
   ```

   Everything public goes through one reverse proxy (Caddy on 8888, see `docs/ports.md`) behind one tunnel: the admin UI and `/api/v1` at `/` (behind OIDC and RBAC), Keycloak at `/auth`, Grafana at `/grafana`. `make demo-share` points a quick tunnel at the proxy (the same as `cloudflared tunnel --url http://localhost:8888`) and, from the URL it prints, sets Keycloak’s `KC_HOSTNAME` (`<url>/auth`), the services’ expected issuer (`<url>/auth/realms/spillway`), the UI’s OIDC authority, Grafana’s `GF_SERVER_ROOT_URL` (`<url>/grafana`, with `GF_SERVER_SERVE_FROM_SUB_PATH=true`) and the `admin-ui` client’s redirect URIs and web origins (through the Keycloak admin API), then restarts what changed, so logins and links use the public address (SC-09). While a share runs, log in through the public URL; localhost logins fail until you stop it. Visitors drive the gateway from the Playground, which generates traffic server-side (UI-07), so the gateway needs no tunnel of its own. Never tunnel an admin port (8001, 8081) or the gRPC stream (9090).
3. Before an interview, rehearse with `docs/demo.md` and keep the recorded video ready as a fallback.
4. Record: terminal scenes as scripted GIFs with `vhs`; UI scenes with OBS Studio (free, all platforms) or QuickTime on macOS.

### Optional: always-on hosted demo on credits

- **AWS Free plan:** $100 credit at signup plus up to $100 more for using services; the plan ends after 6 months or when the credits run out ([AWS](https://aws.amazon.com/about-aws/whats-new/2025/07/aws-free-tier-credits-month-free-plan/)). Only small instance types are eligible; the console labels them.
- **Google Cloud:** new accounts get $300 credit for 90 days. Its always-free e2-micro (1 GB RAM, three US regions only) is too small for the full stack.
- **Steps for either:** sign up with a personal email → set a $1 budget alert → create one Ubuntu VM with ≥ 4 GB RAM and a 30 GB+ disk in a nearby region → add an SSH key → install k3s (`curl -sfL https://get.k3s.io | sh -`) → expose it with a named Cloudflare tunnel (Step 7) so no inbound ports open → delete the VM before the credits end.
- Both need card verification; the local path needs none.

The agent can build all of this except the cloud signup, card check and VM creation; you run demos and record the video.

## Step 7 — Stable public link and hosted dashboards (optional, M8)

A quick tunnel from your laptop (Step 6) is enough for interviews. Do this step only if you want a permanent link, or dashboards that stay up while your laptop is off.

### Stable URL with a named Cloudflare Tunnel

Buying a domain is the one paid item here.

1. Add the domain to a free Cloudflare account and switch its nameservers to Cloudflare’s.
2. Cloudflare dashboard → Zero Trust → Networks → Tunnels → Create a tunnel → copy its token.
3. Add a public hostname for the demo proxy on 8888, the same one the quick tunnel uses: `admin.<domain>` → proxy (UI and API at `/`, Keycloak at `/auth`, Grafana at `/grafana`). No gateway hostname is needed: the Playground drives the gateway server-side (UI-07). Never route a hostname to 8001, 8081 or 9090.
4. Store the token as `CLOUDFLARE_TUNNEL_TOKEN` (Step 8). The agent runs `cloudflared` as a container beside the stack (laptop) or in k3s (cloud VM).

Tell the agent three things: the tunnel is outbound-only, so no inbound ports open; the client IP arrives in `CF-Connecting-IP`, which the control plane must trust only from the demo proxy (SEC-05; the gateway is never tunnelled); and Keycloak’s `KC_HOSTNAME`, the services’ expected issuer and the `admin-ui` redirect URIs must all use the public URL (`https://admin.<domain>/auth`), or logins fail or redirect to localhost (`make demo-share` sets all of them for quick tunnels, SC-09).

A public playground invites abuse. Keep demo mode’s per-session limits (SC-08 in the design doc) and Cloudflare’s free protections on.

### Hosted dashboards on Grafana Cloud

The free tier includes 10k active metric series, 50 GB of logs, 50 GB of traces, 14-day retention and 3 users, with no card required ([pricing](https://grafana.com/pricing/)).

1. Sign up at grafana.com and create a stack in the region nearest you.
2. In the stack: Connections → add an OpenTelemetry (OTLP) connection → generate a token. Note the OTLP endpoint, instance ID and token.
3. Store them as `GRAFANA_CLOUD_OTLP_ENDPOINT`, `GRAFANA_CLOUD_INSTANCE_ID`, `GRAFANA_CLOUD_TOKEN`; the agent adds an exporter to the OTel Collector config.
4. Stay under 10k series: the cardinality caps in OB-01 matter here, so drop runtime metrics no dashboard uses.

Local development keeps the self-hosted Grafana stack from Step 5.

## Step 8 — Secrets and config

Secrets live in three places only: your password manager (source of truth), an untracked `.env.demo` on your machine (`.env.local` holds only dev-safe overrides), and GitHub Actions or Kubernetes secrets for CI and the demo. Never paste a real secret into the agent chat; refer to it by variable name.

1. The agent commits `.env.example` with every variable and dev-safe defaults that are obviously fake (for example `dev-only-not-secret`). The `.gitignore` in the bootstrap commit already covers `.env`, `.env.*` except `.env.example`, `*.pem`, `*.key`, `/kubeconfig*` (repo root), `deploy/certs/` and `.claude/settings.local.json`.
2. You create the two untracked files from it. `.env.local` holds dev overrides and is optional; compose loads it after `.env.example` when it exists (Step 5). Create it with `cp .env.example .env.local`, then change only what you need. `.env.demo` holds the public-demo secrets (M8): `cp .env.example .env.demo`, then fill in real values from your password manager.
3. Generate strong values with `openssl rand -base64 32`.
4. Add CI secrets from your terminal; `gh` prompts for the value, so it never lands in shell history or the chat: `gh secret set GRAFANA_CLOUD_TOKEN`.
5. The agent writes the Kubernetes Secret manifests with placeholders; you create the real ones on the cluster: `kubectl --context kind-spillway create secret generic spillway-secrets --from-env-file=.env.demo`.
6. The agent commits `.gitleaks.toml`, which allowlists only the dev realm’s client secret, the test-user passwords and the `.env.example` defaults. The scan is never weakened otherwise, so a real secret anywhere else still fails the hook and CI.

| Secret | Comes from | Stored in | Needed by |
| --- | --- | --- | --- |
| Local dev passwords and client secret (Postgres, Keycloak admin, test users, `gateway-tests`) | Agent generates | `.env.example` and the realm import; obviously fake values, allowlisted in `.gitleaks.toml` | M0 |
| `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET` | GitHub OAuth app (Step 4) | Password manager, `.env.demo` or k8s secret | M8, optional |
| `POSTGRES_PASSWORD`, `REDIS_PASSWORD`, `KEYCLOAK_ADMIN_PASSWORD` (public demo) | You generate | Password manager, `.env.demo` or k8s secret | M8, optional |
| `CLOUDFLARE_TUNNEL_TOKEN` | Cloudflare (Step 7) | `.env.demo` or k8s secret | M8, optional |
| `GRAFANA_CLOUD_OTLP_ENDPOINT`, `GRAFANA_CLOUD_INSTANCE_ID`, `GRAFANA_CLOUD_TOKEN` | Grafana Cloud (Step 7) | GitHub secret, `.env.demo` | M8, optional |
| `~/.ssh/spillway_cloud` | ssh-keygen, only with a cloud VM (Step 6) | Your machine only | Optional |

Secret scanning has three layers: GitHub push protection (Step 3), a `gitleaks` pre-commit hook installed by `make hooks` (M0) and `gitleaks` in CI on every PR, both reading `.gitleaks.toml`. The Claude Code deny rules in Step 9 sit on top as a guardrail against the agent reading secrets by accident; they are not a security boundary.

## Step 9 — Set up the coding agent

Claude Code needs a Pro, Max, Team, Enterprise or Console account; the free Claude plan doesn’t include it ([setup docs](https://code.claude.com/docs/en/setup)). After login, everything is configuration you write once and commit.

### Install and log in

1. Install (macOS, Linux, WSL2; on Windows run it inside the Ubuntu terminal):

   ```bash
   curl -fsSL https://claude.ai/install.sh | bash     # or: brew install --cask claude-code
   claude doctor                                     # install and settings diagnostics
   ```
2. `cd` into the repo folder and run `claude`, then log in through the browser prompt.

Agent sessions use a plan’s usage allowance much faster than chat. If you hit Pro’s limits mid-milestone, keep sessions to one task (`/clear` between tasks) or move to Max for the heavy build weeks.

### Give the agent the design

Done: `docs/design.md` and `docs/setup.md` are in the repo and are now the source of truth. The claude.ai originals are retired; change these files by PR.

### `CLAUDE.md`

Claude Code reads `CLAUDE.md` at the repo root every session. It is committed and is the source of truth for agent rules; change it by PR. It covers:

- Requirement IDs in `docs/design.md` as the source of truth, with pointers to this guide, `docs/ports.md` and `docs/adr/`.
- The workflow: one milestone at a time, plan before code, tests with every change, `make test lint` before calling a task done, ADRs for decisions, `make verify-m<n>` output in each milestone’s last PR.
- Branches `m<n>-<task>` cut from a freshly fetched `origin/main`, commit subjects like `M1 GW-01: radix-tree router`; the agent never pushes to `main` and never merges.
- The stack and the `make` commands.
- Machine and account rules: org-wide agent conventions loaded from elsewhere don’t apply; always `--context kind-spillway`; `gh` only as `ankitbisht23`; never read or print secrets.
- Known pitfalls: Redis Cluster hostnames and the host address map, AirPlay on 7000, container ulimits, multi-arch images, commands that expand secrets, ports.

### Permissions

`.claude/settings.json` is committed so routine commands run without prompts and the agent doesn’t read secrets by accident. Rule syntax has changed between releases, so check it against code.claude.com/docs/en/permissions for your version.

- **Allow:** build, test and lint (`make`, `go`, `pnpm`, `./bin/*`); `docker compose`, image builds (`docker buildx`) and a few read-only or fault-injection docker commands; git read commands, branches and commits; `gh pr`, `gh run`, `gh workflow`, `gh auth status`; `kind`; `kubectl` and `helm` only with the `kind-spillway` context; k6. `curl` is not allowed, even to localhost, because a URL such as `http://localhost:x@example.com` reaches another host; the agent asks first.
- **Ask:** `git push`, `gh pr merge`, `gh workflow disable`, `docker compose run`, `docker compose exec`.
- **Deny:** reading `.env` files, keys and certs, the mkcert CA directory (`~/Library/Application Support/mkcert` on macOS, `~/.local/share/mkcert` on Linux), `~/.ssh`, `~/.kube`, `~/.npmrc`, `~/.netrc`, `~/.git-credentials`, and gh, docker and cloud credentials; force pushes (`--force`, `-f`, `+branch`); force-adding ignored files (`git add -f`) and skipping hooks (`--no-verify`, `-n`); commands that print secrets expanded: `docker compose config`, `env`/`printenv` through `docker compose exec` or `kubectl exec`, Kubernetes secrets, `helm get values/all/manifest` and `kind get kubeconfig`.

The deny rules are a guardrail, not a security boundary. `go`, `make`, `pnpm` and `docker compose` can run arbitrary code (a test, a Makefile recipe, a package script, a compose service), and that code can read any file your user can. So keep `.env.local` to dev-safe values and real secrets only in `.env.demo` and your password manager.

`.claude/settings.local.json` is gitignored and machine-local: it sets `GH_CONFIG_DIR` and `KUBECONFIG` for agent sessions, so the agent uses the personal `gh` login and only the kind cluster. Create it on each machine, from inside the repo (if the file already exists, add the two `env` keys by hand):

```bash
test -e .claude/settings.local.json || cat > .claude/settings.local.json <<EOF
{
  "env": {
    "GH_CONFIG_DIR": "$HOME/.config/gh-personal",
    "KUBECONFIG": "$HOME/.kube/spillway-kind"
  }
}
EOF
```

If your Claude account has connectors or plugins you don’t want this repo to use (for example a work Slack or tracker), deny them per server in `.claude/settings.local.json` with rules like `"mcp__<server>"` under `permissions.deny`.

A `Read` deny doesn’t stop `cat .env.local` through the shell either. Keeping Bash on an allow-list rather than allowing it wholesale decides which commands run without asking; it doesn’t contain what an allowed command runs, so review the Makefile, test and package-script changes in each PR.

### Working loop per milestone

1. Ask for a plan: “Read CLAUDE.md and docs/design.md. Plan M\<n>: tasks with requirement IDs, files to touch, tests. No code yet.”
2. Edit the plan until you agree with it.
3. “Implement task 1 on branch `m<n>-<task>` off `main`, run `make test lint`, open a PR.”
4. The agent opens the PR and ticks the milestone’s boxes in `docs/design.md` in the PR that completes them. You review it and merge; the agent never merges.
5. The milestone’s last PR carries the output of `make verify-m<n>`, which runs the exit checks and prints each measured number with PASS/FAIL; re-run it yourself before you merge that PR. M1 is the one exception: its baseline comes from a bench run on `main` after the last M1 PR merges, so a small follow-up `M1 exit` PR records it.
6. Run `/clear` between tasks to keep context small; repeat.

## Ready-to-start checklist

When every box below is ticked, the agent can build M0 to M6 without needing you for anything but reviews; M1 adds the first benchmark run; M7 and M8 add kind, nightly benchmarks and the demo; the cloud, Cloudflare domain and Grafana Cloud steps stay optional.

- [ ] `docker run --rm hello-world` works without `sudo`
- [ ] `docker info --format '{{.MemTotal}}'` shows ≥ 8 GB
- [ ] macOS: AirPlay Receiver off (`lsof -nP -iTCP:7000 -sTCP:LISTEN` prints nothing)
- [ ] `go version`, `node -v`, `pnpm -v`, `gh --version`, `k6 version` all succeed
- [ ] `mkcert -install` done
- [ ] Open-files limit raised (`ulimit -n` prints 65536)
- [ ] Repo in a folder iCloud doesn’t sync (inside the Linux filesystem on WSL2)
- [ ] Inside the repo, `git config user.email` prints the no-reply address and the SSH check from Step 3 greets `ankitbisht23`
- [ ] `GH_CONFIG_DIR=~/.config/gh-personal gh auth status` shows `ankitbisht23`
- [ ] `.claude/settings.local.json` exists (Step 9)
- [ ] Ports in `docs/ports.md` free (its check prints nothing)
- [ ] Bootstrap commit reviewed and pushed: `CLAUDE.md`, `.claude/settings.json`, `.gitignore`, `LICENSE`, `README.md`, `docs/design.md`, `docs/setup.md`, `docs/ports.md`, `docs/adr/`
- [ ] `main` protected by a ruleset, turned on after the push; push protection on
- [ ] `claude doctor` clean and `claude` logged in

First prompt to the agent:

> Read CLAUDE.md and docs/design.md. Plan milestone M0: tasks with requirement IDs, files to create, and how we verify the exit check. No code until I approve the plan.

## Costs

The only required spend is the Claude plan; a domain and cloud credits are optional.

| Item | Cost | Limit that matters |
| --- | --- | --- |
| Claude Code | Paid: Pro, Max or Console usage | Free Claude plan excluded ([source](https://code.claude.com/docs/en/setup)) |
| Docker Desktop | Free for personal use | Paid only for companies with ≥ 250 staff or ≥ $10M revenue ([licence](https://docs.docker.com/subscription/desktop-license/)) |
| Go, Node, Postgres, Redis, Keycloak, Prometheus, Grafana, Tempo, Loki, k6, kind, vhs, OBS Studio | Free, open source | Self-hosted |
| GitHub, public repo | Free | 4-vCPU / 16 GB runners free and unlimited in public repos ([runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)); private: 2,000 min/month ([billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions)) |
| Cloudflare quick tunnel | Free, no account | URL changes every run |
| Grafana Cloud (optional) | Free | 10k series, 50 GB logs, 50 GB traces, 14-day retention ([pricing](https://grafana.com/pricing/)) |
| Domain + named tunnel (optional) | Domain roughly $10–15 a year (approximate); tunnel free | Renews yearly |
| AWS Free plan (optional) | $100 credit + up to $100 more | Ends after 6 months or when credits run out ([AWS](https://aws.amazon.com/about-aws/whats-new/2025/07/aws-free-tier-credits-month-free-plan/)) |
| Google Cloud (optional) | $300 credit for 90 days | Always-free e2-micro too small for the stack |

## Sources

Free-tier limits change without notice; recheck these before signing up.

- [Claude Code setup: requirements, install, account types](https://code.claude.com/docs/en/setup)
- [GitHub: hosted runner hardware for public and private repos](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub: how Actions usage is billed](https://docs.github.com/en/billing/concepts/product-billing/github-actions)
- [AWS: Free Tier credits and 6-month free plan](https://aws.amazon.com/about-aws/whats-new/2025/07/aws-free-tier-credits-month-free-plan/)
- [Grafana Cloud pricing](https://grafana.com/pricing/)
- [Docker Desktop licence agreement](https://docs.docker.com/subscription/desktop-license/)
- [Oracle Cloud: Always Free Resources](https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm) (dropped from the plan; kept for reference)

Google Cloud credit and e2-micro figures come from secondary sources; check cloud.google.com/free before relying on them. Cloudflare Tunnel, Keycloak brokering, kind, vhs and the 60-day pause on scheduled workflows are from general knowledge, not pages checked today.
