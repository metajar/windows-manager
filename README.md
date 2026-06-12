# rewardd

A dead-simple time-bank for gating kids' use of a Windows gaming PC. Kids earn
minutes (chores, whatever), you grant the minutes from your phone, and the PC
plays until the bank hits zero. When it does, a fullscreen lock takes over the
desktop until the server says there is more time.

Two binaries, both CGO-free (pure-Go dependencies only), both cross-compile to
a single static executable:

- **server** is the source of truth. It runs as a tiny hardened **Linux
  systemd service** (or anywhere Go runs), holds the time bank, exposes a small
  REST API, and serves a phone-friendly parent dashboard. It persists to JSON or
  SQLite.
- **agent** runs on the Windows gaming PC. It heartbeats the server, counts down
  locally, and throws up the lock overlay when time runs out. It ships as a
  Windows **MSI**.

## Why this shape

The instinct is "make the Windows piece a service." A Windows service runs in
session 0 and physically cannot draw on the interactive desktop, so it cannot
show a lock screen or grab the keyboard. The robust pattern is a brain/face
split: a service for the privileged, hard-to-kill logic, and a per-session
process for the UI and the keyboard hook.

The agent supports **both** deployment models:

- **Standalone (simple).** One per-session process that talks to the server and
  draws the overlay, launched by a logon scheduled task (`-install`). Easiest to
  reason about, but a savvy kid can kill it from Task Manager.
- **Brain/face (hardened).** A **brain** Windows service runs as LocalSystem,
  holds the API token, owns the accounting and the lock decision, and serves
  lock state to a thin **face** UI over a named pipe. The brain launches the
  face into the active desktop and relaunches it if it dies, so killing the UI
  only blanks the screen for a moment — it never buys free play. The MSI installs
  this model.

Both models are fail-closed: the UI starts LOCKED and only unlocks once the
server confirms time remains, so pulling the network cable (or killing the
brain) does not grant free play.

## Install

### Server (Linux, systemd)

Build the binary, then run the installer as root. It creates a dedicated
`rewardd` system user, installs the binary to `/usr/local/bin`, drops a config
file at `/etc/rewardd/server.env` (generating a random `REWARDD_TOKEN` for you),
installs a hardened systemd unit, and starts it.

```sh
make server                       # -> dist/rewardd-server-linux-amd64
sudo ./scripts/install-server.sh  # installs + enables + starts the service
```

```sh
systemctl status rewardd-server
journalctl -u rewardd-server -f
# edit /etc/rewardd/server.env then:
sudo systemctl restart rewardd-server
```

The unit (`build/systemd/rewardd-server.service`) is locked down with
`NoNewPrivileges`, `ProtectSystem=strict`, a private `StateDirectory`, a syscall
filter, and an empty capability set. State (the JSON db) lives in
`/var/lib/rewardd`. The server drains in-flight requests on `SIGTERM`, so
`systemctl restart` is graceful.

### Agent (Windows, MSI)

Install with a double-click, or silently from an elevated prompt with your
settings as properties:

```bat
msiexec /i rewardd-agent.msi /qn ^
  SERVER=http://homelab:8080 TOKEN=your-token KIDUSER=leo PIN=1234
```

The MSI installs `rewardd-agent.exe` to `Program Files\rewardd`, writes the
config to `C:\ProgramData\rewardd\agent.conf`, and installs the **brain service**
(`rewardd-agent-svc`, LocalSystem, auto-start). The brain launches the face UI
into the logged-in session. Uninstalling stops/deletes the service and removes
the config.

| MSI property | Maps to | Default |
| --- | --- | --- |
| `SERVER` (required) | server base URL | |
| `TOKEN` (required) | API bearer token | |
| `KIDUSER` (required) | kid name (matches the server tile) | |
| `MACHINE` | session label | machine hostname |
| `PIN` | local offline emergency PIN | (none) |
| `INTERVAL` | heartbeat seconds | 20 |
| `GRACE` | minutes per offline unlock | 30 |

No MSI? Install either model from an elevated prompt:

```bat
:: hardened brain/face service (what the MSI does)
rewardd-agent.exe -install-service -server http://homelab:8080 -token your-token -user leo -pin 1234

:: or the simple standalone logon task
rewardd-agent.exe -install -server http://homelab:8080 -token your-token -user leo
```

Remove them with `-uninstall-service` / `-uninstall` respectively.

## Build

Go 1.25+. Dependencies are all pure-Go, so builds stay CGO-free and static:
`modernc.org/sqlite` (SQLite backend), `github.com/Microsoft/go-winio` (the
brain/face named pipe), and `golang.org/x/sys` (Windows service + session APIs).
The Windows-only ones never get linked into the Linux server. The `Makefile`
produces static, version-stamped binaries.

```sh
make build      # host-native server + agent into ./bin (dev)
make server     # linux/amd64 server  -> ./dist
make agent      # windows/amd64 agent -> ./dist
make release    # full cross-compile matrix + SHA256SUMS
make test       # race-enabled unit tests
make msi        # Windows agent .msi (needs the .NET SDK + wix tool)
make help       # list everything
```

Version, commit, and build date are stamped into both binaries and surfaced via
`-version` on the CLI and `GET /version` on the server.

```sh
go build -o server ./cmd/server                          # plain build still works
GOOS=windows GOARCH=amd64 go build -o agent.exe ./cmd/agent
```

## Configure

### Server (environment)

The server reads its config from the environment (systemd loads it from
`/etc/rewardd/server.env`; see `build/systemd/server.env.example`).

| Env | Default | Meaning |
| --- | --- | --- |
| `REWARDD_TOKEN` | (required) | Bearer token for API and dashboard |
| `REWARDD_ADDR` | `:8080` | Listen address |
| `REWARDD_DB` | `rewardd.json` | Persistence file path |
| `REWARDD_STORE` | (inferred) | Backend: `json` or `sqlite` (inferred from the `REWARDD_DB` extension if unset) |
| `REWARDD_PARENT_PIN` | (empty) | PIN the lock screen accepts to buy more time |
| `REWARDD_UNLOCK_MIN` | `30` | Minutes granted per successful PIN unlock |
| `REWARDD_HEARTBEAT` | `20` | Heartbeat cadence handed to agents (authoritative) |
| `REWARDD_MAXGAP` | `120` | Max seconds a single heartbeat can charge |
| `REWARDD_SHUTDOWN` | `10` | Graceful shutdown drain timeout (seconds) |

Open the dashboard at `http://that-box:8080/`, paste the token once (it lands in
browser localStorage), and you get a tile per kid with remaining time, a
PLAYING/idle badge, +15/+30/+60 minute buttons, a Lock-now button, and an
add-kid field.

`REWARDD_MAXGAP` is the anti-drain valve: if the PC sleeps, crashes, or the
agent dies for an hour, the next heartbeat charges at most this many seconds
instead of the full wall-clock gap. The bank does not evaporate while the
machine is off.

### Agent (flag / env / config file)

The agent resolves each setting with the precedence **flag > environment
variable > config file > built-in default**. The MSI writes the config file;
CLI users can use flags or env. The config file is a simple `key = value` file
(default `C:\ProgramData\rewardd\agent.conf`, or `/etc/rewardd/agent.conf`
elsewhere); point at another with `-config`.

| Flag | Env | Config key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `-server` | `REWARDD_SERVER` | `server` | (required) | Server base URL |
| `-token` | `REWARDD_TOKEN` | `token` | (required) | Bearer token |
| `-user` | `REWARDD_USER` | `user` | (required) | Kid name, matches the server tile |
| `-machine` | `REWARDD_MACHINE` | `machine` | hostname | Session label for the dashboard |
| `-pin` | `REWARDD_PIN` | `pin` | (empty) | Local emergency PIN, used only when the server is unreachable |
| `-interval` | `REWARDD_INTERVAL` | `interval` | `20` | Heartbeat seconds (server can override) |
| `-grace` | `REWARDD_GRACE` | `grace` | `30` | Minutes granted per offline emergency unlock |
| `-config` | `REWARDD_CONFIG` | | OS default | Path to the config file |

Run modes and one-shot subcommands:

- (no flag) — **standalone**: heartbeat + overlay in one process.
- `-service` / `-face` — run as the **brain** service or the **face** UI client
  (you normally don't run these by hand; the service launches the face).
- `-install` / `-uninstall` — register/remove the standalone logon task.
- `-install-service` / `-uninstall-service` — install/remove the brain service.
- `-writeconfig` — write the resolved settings to the config file and exit.
- `-version` — print version and exit.

The unlock screen tries the server PIN first so grants are banked centrally and
show up on every device. The local `-pin` is a break-glass for when the server
is down; it grants `-grace` minutes locally and nothing is recorded server-side.

## API

All routes except `/healthz`, `/version` and `/` require
`Authorization: Bearer <token>`.

| Method | Path | Body | Returns |
| --- | --- | --- | --- |
| GET | `/healthz` | | `ok` |
| GET | `/version` | | build metadata JSON |
| GET | `/` | | parent dashboard |
| POST | `/api/v1/heartbeat` | `{user, machine}` | `{allowed, remaining_seconds, heartbeat_interval}` |
| POST | `/api/v1/grant` | `{user, minutes\|seconds, reason}` | status |
| POST | `/api/v1/set` | `{user, minutes}` | status (absolute set, not additive) |
| POST | `/api/v1/unlock` | `{user, pin, minutes?}` | `{ok, ...status}` |
| GET | `/api/v1/status?user=` | | status |
| GET | `/api/v1/users` | | all kids |

Wiring chore automation is just a POST:

```sh
curl -s http://homelab:8080/api/v1/grant \
  -H "Authorization: Bearer $REWARDD_TOKEN" \
  -H 'content-type: application/json' \
  -d '{"user":"leo","minutes":30,"reason":"unloaded dishwasher"}'
```

## How the accounting works

The server charges wall-clock time elapsed between heartbeats of the same
session. The first beat of a session is free, so logging in does not cost
anything. Switching machines starts a fresh session and is also free, so a kid
moving from one PC to another is not double-charged. Each charge is capped at
`REWARDD_MAXGAP`. Balance clamps at zero and the session flips to not-allowed,
which is what the agent sees and what triggers the lock.

## Storage

State sits behind a `Store` interface with two interchangeable backends. The
time-bank rules live in one place (`internal/store/accounting.go`) and both
backends call them, so a shared conformance test proves they behave identically.

- **JSON** (default) — the whole world in one file via atomic
  temp-file-plus-rename, mutex-guarded. Perfect for a single household.
- **SQLite** — the pure-Go `modernc.org/sqlite` driver (no CGO), WAL mode,
  one transaction per operation. Use it by pointing `REWARDD_DB` at a
  `.db`/`.sqlite` file or setting `REWARDD_STORE=sqlite`.

Migrate an existing JSON db into the configured store (balances and users; the
audit log and ephemeral session state are not carried over):

```sh
REWARDD_TOKEN=... REWARDD_DB=/var/lib/rewardd/rewardd.db REWARDD_STORE=sqlite \
  rewardd-server -migrate-from-json /var/lib/rewardd/rewardd.json
```

## Lock screen limits

The agent runs in user mode, which bounds what the keyboard hook can swallow:

- Caught while locked: Win keys, Alt+Tab, Alt+F4, Ctrl+Esc.
- **Not** catchable: Ctrl+Alt+Del. That is the Secure Attention Sequence, owned
  by Winlogon, and no user-mode process can intercept it by design. From there a
  kid can reach Task Manager.
- Task Manager itself is disabled with Group Policy, not code. See below.

These are not bugs in the agent, they are OS guarantees. The hardening section
is how you actually close them.

## Hardening

For a kid who knows Task Manager exists:

1. **Use the brain/face service (the MSI default, or `-install-service`).** The
   brain runs as LocalSystem, holds the token and the accounting, decides lock
   state, and relaunches the face if it is killed. A standard-user kid cannot
   stop a LocalSystem service, and killing the face just blanks the screen until
   the brain relaunches it — it does not buy time. This is implemented, not
   aspirational; see `cmd/agent/service_windows.go` and `session_windows.go`.
2. **Standard user account for the kid.** No admin means no service stop, no GPO
   edits, and — with the brain/face model — the API token lives only in the
   LocalSystem service's config, never in a process the kid controls.
3. **Disable Task Manager via GPO.** `gpedit.msc` to User Configuration >
   Administrative Templates > System > Ctrl+Alt+Del Options > Remove Task
   Manager, or the `DisableTaskMgr` registry value under
   `HKCU\Software\Microsoft\Windows\CurrentVersion\Policies\System`. Do this on
   the kid's account, not yours.

In the simpler standalone model the agent holds the token in `agent.conf`, so on
an admin kid account a determined kid could read it and grant themselves time.
The brain/face model plus a standard (non-admin) account closes that.

## Testing

```sh
make test        # go test -race ./...
make test-cover  # tests + coverage summary
```

The domain logic (time accounting across both store backends, config
resolution, HTTP handlers, the client, the countdown, and the brain/face IPC
protocol) is unit-tested without needing Windows. Only the Win32 overlay, the
named-pipe transport, the service plumbing, and the session launcher are
untested, by necessity — they are isolated behind build-tagged files and
cross-compiled in CI.

## Project layout

```
cmd/server/main.go          REST API, auth, accounting endpoints, graceful shutdown, migration
cmd/server/main_test.go     handler/auth tests via httptest
cmd/server/dashboard.go     embedded single-file parent dashboard
cmd/agent/main.go           run modes, heartbeat loop, local countdown, PIN submit
cmd/agent/config.go         layered config (flag/env/file) + writeconfig
cmd/agent/ipc.go            brain/face named-pipe protocol (newline JSON)
cmd/agent/controller.go     thread-safe state bridge between net and UI
cmd/agent/lock_windows.go   Win32 overlay + keyboard hook + standalone install
cmd/agent/lock_other.go     dev stub so the agent runs on Linux/macOS
cmd/agent/service_windows.go    brain: SCM handler, pipe server, face watchdog, install/uninstall
cmd/agent/face_windows.go       face: pipe client mirroring brain state into the overlay
cmd/agent/pipe_windows.go       named-pipe listen/dial (go-winio)
cmd/agent/session_windows.go    launch the face into the active desktop from session 0
cmd/agent/service_other.go      non-Windows stubs for the above
cmd/agent/*_test.go         config, client, controller, countdown, IPC tests
internal/store/store.go     Store interface + JSON backend
internal/store/sqlite.go    SQLite backend (modernc, WAL)
internal/store/accounting.go    shared, pure time-bank rules
internal/store/open.go      backend selection + JSON->SQLite migration
internal/store/*_test.go    per-backend + shared conformance tests
internal/buildinfo/          version metadata stamped at build time
build/systemd/              hardened unit + env example for the Linux server
build/msi/                  WiX source + build script for the Windows agent MSI
scripts/install-server.sh   one-shot Linux server installer
Makefile                    static cross-compile, version stamping, packaging
.github/workflows/          CI (vet/race/cross-compile) + tagged releases
```
