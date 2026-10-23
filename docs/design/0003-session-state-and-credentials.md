# 0003: Session state, credential storage and token refresh

- **Status:** Draft, 2026-10-23
- **Builds on:** 0001 agent architecture (v3), 0002 security audit
- **Scope:** Where the agent keeps its identity and tokens, how it refreshes
  them without ever getting itself revoked, and how this works across
  Linux, macOS, Windows, Docker, system services and an interactive terminal.

## 1. Summary of decisions

1. **One credentials file, one atomic replace.** Identity keys, the rotating
   refresh token and the refresh intent live together in
   `credentials.json` in the data directory. It is written by temp file,
   fsync, rename and directory fsync, so a crash leaves the old file or the
   new one and never a mix.
2. **The OS account is the security boundary, not an OS keyring.** The
   agent runs as a dedicated user (or container user), and the data
   directory is owner-only. Platform encryption is layered on top only
   where it works without a logged-in human: DPAPI on Windows today,
   others later (§4).
3. **The access token is memory-only and refreshed lazily.** Freshness is
   checked against the wall clock on every use. There is no refresh timer,
   because Go timers stop during sleep on Linux and macOS (§5.2).
4. **Refresh is crash-safe through a write-ahead nonce.** The agent
   persists `{nonce, refresh_token}` before spending the token. The cloud
   answers a retry with the same token and nonce idempotently. This is a
   **required change to cloud-api** (§6).
5. **One process owns a data directory.** An exclusive OS file lock
   enforces it. A TUI or CLI talks to the running daemon over local IPC
   and never opens a second cloud session (§8).
6. **Signing inputs are length-prefixed.** The `‖` concatenation sketched
   in 0001 is ambiguous. This is a **required change to cloud-gateway and
   cloud-api** (§7).

## 2. Run modes

The agent is a single static binary. What changes between modes is who
starts it, where state lives, how the pairing code reaches a human and
which log format is useful.

| Mode | Started by | Data dir | Pairing code shown via | Logs | Stop signal |
|---|---|---|---|---|---|
| Terminal, foreground | user | per-user (below) | stdout box + log line | text, colour | Ctrl-C |
| Linux system service | systemd, `DynamicUser` | `/var/lib/nabbed-agent` (`StateDirectory`) | `journalctl -u nabbed-agent` | JSON | SIGTERM |
| macOS | launchd agent or daemon | `~/Library/Application Support/Nabbed Agent` or `/Library/...` | log file or `agent pair` first | JSON | SIGTERM |
| Windows | console or Windows service | `%LOCALAPPDATA%\Nabbed\Agent`, or `%ProgramData%\Nabbed\Agent` for a service | console, or `agent pair` first | text or JSON | Ctrl-C or service stop |
| Docker / Compose | container runtime | `/data` named volume | `docker logs` | JSON (no TTY) | SIGTERM |
| Kubernetes | Deployment, 1 replica | PVC at `/data` | `kubectl logs` | JSON | SIGTERM |
| TUI | user, attached to a daemon | none of its own | the daemon's state | n/a | n/a |

Implemented now:

- **`agent run` pairs when unpaired.** Headless installs need no separate
  step. The code appears as a structured log line with `user_code` and
  `verification_uri`, and as a plain-text box on stdout.
- **`agent pair` can run first.** It uses the same data directory
  (`docker exec`, or `sudo -u` for a service account), then the daemon is
  started.
- **Exit code 3 means "pair again".** It covers a revoked agent and
  rejected credentials. The systemd unit uses `RestartPreventExitStatus=3`
  so it stops restarting. Docker users should prefer `restart: on-failure`
  with a retry cap, or `unless-stopped` and alerting on the log line.

## 3. State inventory

| State | Where | Lifetime | Sensitivity | Notes |
|---|---|---|---|---|
| Ed25519 identity seed | `credentials.json` | install | **secret** | Signs Hello and refresh. Token `cnf` binds to its thumbprint. |
| X25519 seal key | `credentials.json` | install, until key rotation (0001 phase 4) | **secret** | Opens sealed source configs. |
| Refresh token | `credentials.json` | single use, rotated on each refresh (each gateway `TOKEN_EXPIRING` rotation while connected), 90-day idle expiry | **secret** | Hashed server-side, reuse revokes. |
| Pending refresh `{nonce, token, started_at}` | `credentials.json` | seconds | secret (contains the token) | Write-ahead record (§6). |
| agent_id, tenant_id, api_url, gateway_url, paired_at, refreshed_at | `credentials.json` | install | low | gateway_url validated at pairing (same eTLD+1 as api_url). |
| Access token (ES256 JWT, 30 min) | memory | 30 min | **secret** | Never written to disk or logs. |
| instance_id | memory | process | low | Random per process start. Fencing key. |
| session_id, heartbeat seq, clock offset | memory | stream | low | Offset learned from `HeartbeatAck.server_time_ms`. |
| last_command_id, applied_commands, outbox, downloads, inventory | SQLite (`state.db`, 0001 §7) | durable | medium | Not secret. Kept out of the credentials file so DB backups and diagnostics never carry keys. |
| Local policy (allowed roots, ceilings, TLS pins) | `policy.yaml`, host-owned | host owner's choice | low | Read by the agent, never written after pairing. Mount read-only in Docker. |
| Unsealed source credentials | memory | while in use | **secret** | Unsealed on demand, never persisted in plaintext. |

`state.db` is bound to the agent id that created it. A database left by a
previous identity, for example after `pair --force`, is moved aside as
`state.db.<old-agent-id>.<unix>.bak` and a new one is created.

`fresh` in Hello is true until the first command has been applied to this
database. If a rehydrate is interrupted before any command lands, the
agent says `fresh` again and the cloud simply repeats it. Once one command
has landed, the rest of the rehydrate arrives from the durable command log.

The database runs in WAL mode with `synchronous=FULL`. A power cut must
not roll back a command whose result the cloud has already acknowledged.
One connection serialises all access, and `agent status` reads through a
separate read-only connection, so it is safe while the daemon runs.
Command results are pruned after 7 days, matching the cloud log's
retention, but the newest is always kept for `last_command_id`.

## 4. Credential storage

### 4.1 Threat model

| Threat | Defended by |
|---|---|
| Other local users reading credentials | Owner-only data dir: 0700 or a protected DACL. File 0600. |
| Backups, snapshots and disk images leaking keys | Platform protector where available (DPAPI binds to machine and account). Elsewhere, guidance to exclude the data dir from backups. |
| Copied or cloned data dir (VM template, volume copy) | Refresh rotation: the second copy to refresh trips reuse detection and the agent is revoked. Gateway fencing (`SUPERSEDED`) makes concurrent use visible in logs. |
| Stolen access token | 30-minute lifetime, and Hello must be signed by the key in `cnf`. A token alone cannot open a stream. |
| Malware running as the agent's user | **Not defended.** No software-only scheme on any of the three OSes stops a same-user process (§4.2). Hardware-bound keys are the future answer (§4.4). |
| Compromised cloud | Out of scope here. 0001 local policy and sealed sources bound it. |

### 4.2 Why not the OS keyring by default

Research into the cgo-free option, `zalando/go-keyring` v0.2.8, plus each
platform's behaviour:

- **macOS Keychain.** Without cgo, access goes through `/usr/bin/security`
  (go-keyring drives `security -i` over stdin, so the secret stays out of
  argv). The item's access list then trusts `security`, not the agent, so
  any process of the same user can read it back with the same CLI without
  a prompt. A launchd daemon running as root needs the System keychain.
  A per-user agent's keychain is locked until that user logs in, which
  breaks start-at-boot.
- **Linux Secret Service.** Needs a D-Bus session bus and an unlocked
  collection (gnome-keyring or KWallet). Neither exists for a systemd
  system service, a headless NAS or a container. When it exists, any
  process on the session bus can read the item.
- **Windows Credential Manager.** Works, but stores per user, protected by
  that user's DPAPI key, with no per-application isolation. Calling DPAPI
  directly gives the same protection with fewer moving parts and also
  works under LocalSystem.

The keyring is unavailable in most deployments the agent targets. Where it
is available, it adds at-rest encryption but no isolation from same-user
processes. A file in an owner-only directory, owned by a dedicated service
account, gives equal isolation everywhere and works identically in every
mode. This is the same model as SSH host keys and the kubelet's client
certificates.

### 4.3 Layers as implemented

1. **Directory.** On Unix, created 0700 and re-chmodded if found looser,
   with a warning if that fails, as on some NAS and NTFS mounts. On
   Windows, a protected DACL `D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)`
   grants SYSTEM, Administrators and the owner only, and blocks
   inheritance from the parent.
2. **File.** 0600 via `CreateTemp`, `Sync`, `Rename` and directory
   `Sync`. Load warns if group or other bits are set.
3. **Envelope.** `{"format":1,"protector":"none|dpapi","data":...}`. The
   protector is recorded per file, so switching defaults never strands
   existing installs. An unknown protector is a hard error that names it.
4. **Protector.**
   - **Windows: DPAPI.** `CryptProtectData` with `CRYPTPROTECT_UI_FORBIDDEN`
     and fixed entropy, called through `golang.org/x/sys/windows` without
     cgo. It is bound to the account the agent runs as, so a copied data
     dir does not open elsewhere.
   - **macOS, Linux and containers: none.** The file is plaintext JSON
     protected by layers 1 and 2.

Logging: `Credentials` implements `slog.LogValuer` and logs only ids, URLs
and whether a refresh is pending. A test asserts that secrets never appear.

### 4.4 Future protectors

These are pluggable behind the same `Protector` interface, and each needs
only a new name in the envelope:

| Protector | Platform | What it buys | Cost |
|---|---|---|---|
| `systemd-creds` | Linux with systemd ≥ 250 | A key-encryption key sealed to the host TPM2, or to `/var/lib/systemd/credential.secret`, delivered via `LoadCredentialEncrypted=` into `$CREDENTIALS_DIRECTORY`. The data dir alone is useless off-host. | Unit-file setup. The KEK is static, which fits, because the rotating file stays on disk. |
| `kek-file` | Docker, Kubernetes | A KEK from a Docker or Kubernetes secret file (`NABBED_AGENT_KEK_FILE`) wraps the credentials with XChaCha20-Poly1305. A leaked volume backup is useless without the secret. | The operator manages one more secret. |
| `keychain` | macOS, user installs | A KEK in the login keychain via `security`. Protects Time Machine copies. | Same-user caveat from §4.2. Locked before login. |
| Hardware-bound identity | Windows (TPM via CNG), macOS (Secure Enclave), Linux (TPM2) | The Ed25519 key is replaced by a **non-exportable** P-256 key. Clone-proof and malware-resistant: stealing the file gets nothing. | Needs cgo or heavy syscall work on macOS. The cloud must accept ES256 proofs alongside Ed25519. Revisit after phase 3. |

Not chosen: a passphrase-encrypted file. It breaks unattended start, which
is the main use case.

**Docker and Kubernetes rule:** credentials go in a **persistent volume,
never in a Secret or environment variable.** The refresh token rotates, so
the agent must write it. A read-only Secret would hold a token that is dead
after the first refresh. Mounting the same volume into two containers
gets the agent revoked: the file lock prevents it only when both see the
same lock, which is not true across hosts or many network filesystems.

## 5. Token lifecycle

### 5.1 Access token

- **Received** at pairing (`/api/device/token`) or refresh. Only `jti`,
  `iat` and `exp` are parsed, without verification. The agent is not the
  audience, and the gateway verifies.
- **Expiry** is computed as `received_at + (exp − iat)` in local time, not
  `exp` itself. A host clock that is hours off still refreshes on schedule.
- **Fresh** means more than `min(5 min, lifetime/4)` remains. With
  30-minute tokens, the agent refreshes during the last 5 minutes.
- **Used** at stream open, as the `authorization` metadata, and later for
  `GET /api/agent/payloads/{id}`. The stream authenticates once. When the
  token nears expiry, the gateway sends `GoAway{TOKEN_EXPIRING}` and the
  agent reconnects with a fresh one.
- **Invalidated** on `Unauthenticated` from the gateway, or on
  `GoAway{TOKEN_EXPIRING}`. Invalidation is compare-and-clear, so a stale
  error cannot discard a newer token.

### 5.2 Why no refresh timer

Research into Go 1.26 runtime sources shows the monotonic clock behind
`time.Timer` behaves differently on each OS:

| OS | Monotonic source | Advances during sleep |
|---|---|---|
| Linux | `CLOCK_MONOTONIC` | no |
| macOS | `mach_absolute_time` | no |
| Windows | interrupt time | yes |

A "refresh in 25 minutes" timer set before a laptop sleeps for two hours
fires about 25 awake-minutes after wake on Linux and macOS, long after the
token died. A check at the moment of use, against the wall clock, is
correct everywhere and needs no goroutine. The link layer needs a token
only at stream open, and after wake the old stream is dead anyway: missed
heartbeat acks or a TCP reset force a reconnect, and that reconnect asks
for a token.

### 5.3 Refresh token

- Single use, rotated on every refresh, with a 90-day idle expiry. A
  connected agent refreshes whenever the gateway rotates its stream with
  `GoAway{TOKEN_EXPIRING}`, roughly once per access-token lifetime, so only
  an agent offline for 90 days needs re-pairing. The gateway must send
  `TOKEN_EXPIRING` before expiry. If it does not, the stream outlives the
  token and refresh happens only at the next reconnect.
- Every refresh carries Ed25519 proof of possession over
  `(api_host, refresh_token, timestamp_ms, nonce)`. A stolen refresh token
  is useless without the identity key.
- Refreshes are serialised in-process by a mutex, so concurrent callers
  share one refresh. Across processes, the data-dir lock (§8) serialises
  them.

### 5.4 Clock skew

`HeartbeatAck.server_time_ms` updates a process-wide offset. The offset
corrects `Hello.signed_at_ms` and the refresh `timestamp_ms`, so a host
whose clock is minutes off stays inside the server's freshness window. The
agent logs a warning when the offset exceeds 2 minutes. On the very first
connect after boot no offset is known yet. A badly skewed clock can then
fail the first Hello, and the retry after one heartbeat round trip
succeeds.

## 6. Crash-safe refresh rotation

### 6.1 The problem

0001 specifies "reuse of a rotated token revokes the agent." That is
standard refresh-token rotation, but combined with a crash it revokes
healthy agents:

```
agent                         cloud-api
  | POST /refresh {rt1}  ---->  | rotates rt1 -> rt2, marks rt1 spent
  |  X  process killed, or response lost, or disk full on save
  | (restart, still holds rt1)
  | POST /refresh {rt1}  ---->  | rt1 is spent: REUSE, revoke agent
```

Power cuts, OOM kills, `docker stop` timeouts and flaky NAS links all hit
this window. An agent revoked after a power cut is a support ticket and a
re-pair.

### 6.2 Agent side (implemented)

```
1. if pending == nil or pending.refresh_token != refresh_token:
       pending = {nonce: random 128-bit, refresh_token, started_at}
       save(creds with pending)                      # durable BEFORE spending
2. sig = Ed25519(refresh_input(api_host, pending.refresh_token, now_ms, pending.nonce))
3. POST /api/device/refresh {refresh_token, timestamp_ms, nonce, signature}
4. on success: save(creds with new refresh_token, pending = nil)
   on permanent error (invalid_grant, agent_revoked, 401, 403): ErrReauthRequired, exit 3
   on transient error: keep pending and retry later with the SAME nonce
```

The retry signs a new timestamp but reuses the nonce. If the save in step
4 fails, the agent keeps the new token in memory and the pending record
still allows an idempotent retry. Tests cover a response dropped after
rotation, a "restart" from disk, and a clone using a different nonce, which
must still be revoked.

### 6.3 Required cloud-api change

This is the server half of the contract. Without it, §6.2 alone cannot
prevent the revocation.

1. When rotating `rt1 → rt2`, store
   `{hash(rt1), nonce, response: {access, rt2}, expires: now + 10 min}`.
2. On refresh with a spent token:
   - Same nonce, within the grace window, and the PoP signature verifies:
     return the **stored response**. Do not mint new tokens and do not
     revoke.
   - Different nonce, or outside the window: this is **reuse**, so revoke
     the agent (`agent_revoked`).
3. Freshness: reject `timestamp_ms` more than 5 minutes from server time
   with `invalid_request`, which is retryable and does not revoke. Keep
   `(agent, nonce)` until the timestamp window passes, to stop replay of
   a captured request.
4. Once `rt2` is used, either for a refresh or as the access token's
   parent on stream open, drop the replay entry for `rt1`.

This mirrors the reuse interval that mainstream identity providers offer
for rotating refresh tokens. It is tighter, because the retry must carry
the same nonce and a valid signature from the bound key.

### 6.4 What clones look like

Two hosts with the same data dir both hold `rt1`. The first to refresh gets
`rt2`. The second sends `rt1` with its own fresh nonce, which is reuse, and
the agent is revoked. Both hosts exit with code 3 and the UI shows "agent
revoked: refresh token reuse." This is intentional. Clone detection is the
main reason to keep rotating refresh tokens rather than minting tokens
from the key alone (§9).

## 7. Session state machine and protocol notes

```
            pair ok                   Welcome
Unpaired ----------> Paired --------> Connecting ---------> Connected
   ^  (run auto-pairs)                   ^   |                 |
   |                                     |   | error            | GoAway / stream error /
   |                                     |   v                 | 3 missed acks / max age
   |                                  Backoff <----------------+
   |                                     |
   +----- exit 3 (NeedsRepair) <---------+  REVOKED, PermissionDenied, refresh rejected
```

- **instance_id** is random per process and stable across reconnects.
  After a restart the new instance supersedes the old session at the
  gateway. A `SUPERSEDED` GoAway is logged as "another process connected
  with this identity", which usually means a copied data dir.
- **Backoff** is uniform in `[1s, min(60s, 1s·2^n)]`. It resets after a
  session stays up for 60 seconds. GoAway's `reconnect_after_ms` is
  honoured with up to 20% jitter, and `DRAIN` or `TOKEN_EXPIRING` without
  a delay reconnects within one second. Reaching max stream age
  reconnects immediately.
- **Heartbeat** runs at the `Welcome.heartbeat_ms` interval, 15 seconds by
  default. Three unacked heartbeats end the stream.
- **Ordering:** one goroutine reads and one writes. Commands are applied
  sequentially by a worker. Queries run in a pool of 4, each with a
  15-second deadline, and answer "agent busy" when the pool is full. Send
  lanes are prioritised: control (results and heartbeats), then events,
  then telemetry (latest value per key).

### 7.1 Required protocol changes to 0001

| Change | Why | Status |
|---|---|---|
| Length-prefix every field of the Hello and refresh signing inputs: `u32be(len) ‖ bytes` for the label and each field, with timestamps as 8-byte big-endian | `"ab" ‖ "c"` equals `"a" ‖ "bc"`. With attacker-influenced fields (`jti`, `nonce`), plain concatenation lets one signature verify for a different tuple. | Implemented in `internal/identity`, with a byte-layout test vector. Gateway and api must match. |
| Add `Hello.signed_at_ms` | 0001 signs `timestamp_ms` but does not say which field carries it. Header timestamps are not covered by the signature. | In proto (field 9). |
| Refresh idempotency (§6.3) | Crash safety | Cloud work. |
| Public keys at `/authorize` are base64url without padding | 0001 leaves the encoding open. This matches JWK `x`. | Implemented. |
| Gateway returns `PermissionDenied` for revoked agents at stream open | Lets the agent stop immediately instead of backing off forever | Agent treats it as revoked. |
| Proof of possession on `GET /api/agent/payloads/{id}` (a DPoP-style signed header over method, URL, timestamp and the token's `jti`) | Today a leaked 30-minute access token can fetch that agent's payloads. This makes the payload path match the stream. | Recommended, not implemented. |

## 8. One owner per data directory, TUI included

Two processes using one credentials file will eventually refresh
concurrently, and that is reuse. So:

- **`run` and `pair` take an exclusive OS lock** on `agent.lock` in the
  data dir (`flock` on Unix, `LockFileEx` on Windows). The OS releases it
  on process death, so there is never a stale lock. A second `run` fails
  fast with "data directory is in use by another agent process."
- **`status` reads without the lock.** Atomic replace makes concurrent
  reads safe.
- **The TUI is a client of the daemon, not a second agent.** The daemon
  will expose a local control endpoint in the data dir: a Unix socket at
  0600 on Linux and macOS, and a named pipe `\\.\pipe\nabbed-agent-<hash>`
  with an owner-only DACL on Windows. It is read-mostly: status, sessions,
  downloads, logs, pending source approvals and `sources approve`. The TUI
  never touches `credentials.json` and never opens a gRPC stream. When no
  daemon runs, `agent tui` offers to start one in the foreground and
  attach, which is the "run it in a terminal" mode. Not implemented yet.

## 9. Alternatives considered

| Alternative | Verdict |
|---|---|
| **Keyring by default** | Rejected. Unavailable headless. No isolation from same-user processes when available (§4.2). |
| **Drop refresh tokens; mint access tokens from a key-signed assertion** (RFC 7523 `private_key_jwt`) | Attractive: no rotating secret, no crash window, nothing on disk changes after pairing. Rejected for now because it loses clone detection (§6.4), and cloud-api already has rotation. Worth revisiting together with hardware-bound keys, which make cloning impossible anyway. |
| **Background refresh timer** | Rejected (§5.2). |
| **Persist the access token** | Rejected. It saves one refresh per restart and adds a secret to disk for 30 minutes. |
| **Credentials in SQLite next to state** | Rejected. Backups, `.dump` and diagnostic bundles would carry keys, and SQLite's WAL leaves old pages, including old refresh tokens, until checkpoint. |
| **Kubernetes Secret or Docker secret for credentials** | Rejected. The refresh token rotates, so it must be writable (§4.4). Fine for a static KEK. |

## 10. What this bootstrap implements

| Package | Contents |
|---|---|
| `proto/nabbed/agent/v2` | The full 0001 contract, generated into `pkg/proto/nabbed/agent/v2` |
| `internal/identity` | Ed25519 and X25519 keys, the RFC 7638 thumbprint (RFC 8037 test vector), length-prefixed signing inputs |
| `internal/credstore` | Atomic, owner-only credentials file, envelope, DPAPI protector on Windows, redacted logging |
| `internal/lock` | Data-dir lock |
| `internal/auth` | Device flow client (RFC 8628 errors and `slow_down`), lazy token source with crash-safe rotation, gateway URL check (eTLD+1) |
| `internal/link` | `Connect()` session: signed Hello, Welcome deadline, heartbeats, GoAway handling, backoff, ordered command worker, query pool, prioritised send lanes, clock offset |
| `internal/tlsconf` | System roots plus `ca_file`, SPKI pins |
| `internal/store` | SQLite (pure Go) with embedded migrations, agent-id binding, exactly-once `ApplyCommand` (state change and result in one transaction), outbox written in the same transaction as state changes, read-only `Inspect` |
| `internal/command` | Applier implementing the link handler on the store. Every command kind is still rejected `UNSUPPORTED`, but the rejection is recorded and replayed. |
| CLI | `pair`, `run` (auto-pairs), `status`, exit code 3 for re-pair |
| Packaging | Distroless nonroot `Dockerfile`, hardened systemd unit |

Not yet implemented:

- Command kinds: each is rejected `UNSUPPORTED` until its applier and its
  tables (roots, sources, downloads, inventory) land.
- Queries, including `GetStatus`.
- `policy.yaml`, and wiring SPKI pins into it.
- Windows service wrapper (`x/sys/windows/svc`) and launchd plist.
- Local control socket and the TUI.
- systemd-creds and KEK protectors.

## 11. Open questions

1. **Grace window length for §6.3.** 10 minutes covers restart-on-failure
   loops. Should the window close as soon as `rt2` is first used, or only
   on expiry?
2. **Windows service account.** Should the default be LocalSystem, a
   virtual service account (`NT SERVICE\nabbed-agent`) or a managed
   account? A virtual account plus `%ProgramData%` with an explicit DACL
   is recommended, for least privilege with no password to manage.
3. **Unattended enrolment** for fleet or Kubernetes installs: a pre-issued,
   single-use enrolment token (`NABBED_ENROLL_TOKEN`) that replaces the
   device flow. This is cloud work. It must still bind the agent-generated
   key at first use.
4. **Re-pair UX.** `pair --force` currently generates a new identity, which
   orphans the old agent record and requires re-sealing sources. Should
   the cloud offer "replace agent X" so roots and settings carry over?
