# 0006: Usenet sources from the control plane

- **Status:** Implemented in the agent, 2026-10-24, matching the web app's
  existing sealed format (nabbed-frontend `seal.ts`, schema version 1).
- **Scope:** `PutSource` and `RemoveSource` with `kind = "usenet"`: how the
  web app seals provider credentials, what the agent checks, and what the
  cloud must handle.

## Contract

### Sealing (web app)

The browser seals the configuration with libsodium `crypto_box_seal` to the
agent's X25519 public key. The web app reads that key from GraphQL
`Agent.unsealPublicKey`, served as standard padded base64 of the raw 32
bytes. (The agent sends it to `POST /api/device/authorize` as base64url
without padding; the cloud decodes that and stores the raw bytes.)

The cloud stores the sealed bytes per agent and cannot open them. It
forwards the **same bytes** on every `PutSource` for that source,
rehydration included. Sealing again produces new bytes, which the agent
treats as a changed source and reconnects that provider's pool for.

The agent's test `TestOpensLibsodiumSealedBox` opens a box of this shape
made by libsodium 1.0.20 through libsodium-wrappers.

### `Source` fields

| Field | Rule |
|---|---|
| `id` | 1–64 of `A-Z a-z 0-9 _ -`. The cloud uses UUIDs. |
| `kind` | `"usenet"`. Other kinds (`sabnzbd`, `nzbget`, `qbittorrent`, `transmission`, `torrent`) get `SOURCE_KIND_UNSUPPORTED` for now. |
| `display_name` | Up to 128 bytes (the cloud caps it at 100); logs only. |
| `sealed_config` | The sealed box, 1 byte–16 KiB. |
| `sealed_to_key` | The raw 32-byte X25519 public key it was sealed to. If it differs from the agent's key: `SOURCE_UNSEALABLE`, "sealed to a different agent key". |
| `enabled` | `false` keeps the source stored but unused. |

### Sealed plaintext, schema version 1

UTF-8 JSON, one object, as `seal.ts` produces it. Unknown fields are
rejected, so a misspelling fails at `PutSource` instead of as an
authentication failure mid-download.

```json
{
  "schema_version": 1,
  "host": "news.newshosting.com",
  "port": 563,
  "tls": true,
  "insecure_skip_verify": false,
  "credentials": { "username": "user", "password": "secret" },
  "connections": 30
}
```

| Field | Rule |
|---|---|
| `schema_version` | Must be `1`. |
| `host` | DNS name or IP. Must not be local: private, loopback, link-local, CGNAT, an address with a zone (`fe80::1%eth0`), `localhost`, `*.local`, `*.lan`, `*.internal`, `*.home.arpa` → `SOURCE_HOST_BLOCKED`, with a message naming `network.allowed_ranges`. The exception is an address in the host owner's `network.allowed_ranges`; local names stay refused. Checked again after DNS at every dial, against the same ranges. |
| `port` | 1–65535, default 563. |
| `tls` | Must be `true`. |
| `insecure_skip_verify` | Must be `false` or absent. Without certificate checks, TLS does not protect the password. |
| `credentials` | `{username, password}`: both or neither, no line breaks. Omit for an open server. `api_key` is rejected for usenet. |
| `base_path` | Ignored for usenet (used by HTTP download clients). |
| `connections` | Optional agent extension, 1–100, default 8. The account limit, shared by all downloads; the agent lowers it if the provider refuses connections. The web form does not send it yet. |

Backup (fill-only) providers will come from a plain `role` field on
`Source` (proposal agreed, not yet in the proto). Until then every source
from the web app is a primary.

### Results

| Outcome | When |
|---|---|
| `APPLIED` | Stored; the provider is live after commit. Also for `RemoveSource` of an unknown id, so removals can be replayed. |
| `REJECTED` + `SOURCE_KIND_UNSUPPORTED` | `kind` is not `usenet`. |
| `REJECTED` + `SOURCE_UNSEALABLE` | Wrong key, tampered or truncated box. |
| `REJECTED` + `SOURCE_HOST_BLOCKED` | Local host. |
| `REJECTED` + `INVALID_ARGUMENT` | A field is missing or out of range; the message names it. |
| `REJECTED` + `LIMIT_EXCEEDED` | More than 32 sources. |

Result messages never contain secrets and are safe to show users.

### Capabilities

Capabilities are only sent in `Hello`. When a command changes them (the
first enabled source adds `download.usenet`; removing the last takes it
away), the agent sends the `CommandResult`, then closes the stream and
reconnects at once with a new `Hello`. That reconnect is not `fresh`, so it
does not trigger rehydration.

### Reconciliation on every Hello

Commands queued while the agent was offline arrive after it reconnects,
but a queue only replays what was sent. Restoring an older data directory,
a failed command, or a lost queue would leave the agent and the cloud
disagreeing, and nothing would notice. So the agent reports what it holds,
and the cloud brings it in line on every `Hello`, not only `fresh` ones.

With the capability `sources.inventory`, `Hello.local_sources` is the
complete list of sources the control plane has put on the agent, in the
order they were first stored. Sources configured on the agent itself
(`NABBED_NNTP`) are never listed. Each entry has:

| Field | Meaning |
|---|---|
| `id`, `kind` | As in the `Source`. |
| `sealed_config_sha256` | SHA-256 of `sealed_config` exactly as received. It reveals nothing about the contents. |
| `enabled` | As last put. |
| `usable` | False when the stored box no longer opens, for example after re-pairing. |

The cloud compares this list with the sources it holds for the agent:

| Agent reports | Cloud holds | Cloud sends |
|---|---|---|
| Nothing for the id | Source | `PutSource` |
| Different hash or `enabled` | Source | `PutSource` with its stored bytes |
| `usable: false` | Source | Nothing; mark `needsReseal` |
| The id | Nothing (deleted) | `RemoveSource` |
| Same hash and `enabled` | Source | Nothing |

Both commands are idempotent and an unchanged `PutSource` keeps the live
pool, so reconciling after the queue has already delivered is harmless.
Without the capability (older agents), `local_sources` is empty and means
nothing; the cloud must not send removals based on it.

The list is read from the agent's store when each `Hello` is built. If that
read fails, the agent does not send the `Hello` and retries the connection
instead, because a partial list would look like sources are missing.

Observed on 2026-10-24 against production, with no sources in the cloud:
the gateway accepted the `Hello` carrying `sources.inventory` and an empty
list, and sent nothing back. The agent also still reports `fresh: true`,
because it has never applied a command, so every connect is treated as a
first connection.

## Cloud-side status

Confirmed by the cloud session on 2026-10-24:

- Sealing, raw `sealed_to_key`, per-agent rows (`agent_sources.agent_id`)
  and resending stored bytes on rehydration already match.
- Re-pairing updates the agent's `unseal_public_key`;
  `DownloadSource.needsReseal` lets the UI ask for credentials again.

Open items for the cloud:

0. **Reconcile against `local_sources`** on every Hello whose
   capabilities include `sources.inventory` (see above).

1. **Skip stale sources on rehydration.** Rehydrate still sends
   `PutSource` for sources with `needsReseal`; each gets
   `SOURCE_UNSEALABLE`. Harmless but noisy.
2. **Check capabilities before dispatching.** Send a usenet
   `StartDownload` only while the latest `Hello` lists `download.usenet`;
   today any enabled usenet source is enough.
3. **Show rejected sources.** A rejected `PutSource` is only on the command
   row; mark the source row unhealthy too.
4. **Add `connections` to the usenet form** and the sealed object.
   Providers allow far more than the default 8.
5. **Hide `insecure_skip_verify` for usenet,** since the agent rejects it.
6. **Proto additions (agreed):** `int32 priority` and a plain `role`
   (`PRIMARY`, `BACKUP`) on `Source`.

### Health: `SourceHealth` events and `TestSource`

The agent reports each source's health so the web app can show it.

- **When a source is applied** (first `PutSource`, a changed one, or at
  startup), the agent opens one connection: TCP, TLS, greeting and login.
  The connection is then kept for downloads. If downloads already hold
  working connections, they count as the check, so the account limit is
  never exceeded.
- **During downloads** the state follows real use. A good connection makes
  it healthy. A login failure or blocked address makes it unhealthy at
  once. Five failed connection attempts in a row make it unhealthy, and
  one success recovers it.
- **Failing sources are checked again** in the background after 1 minute,
  then 2, 4 and so on up to an hour between attempts, so the status
  recovers without a download needing the provider. The cap keeps failed
  logins rare enough that a provider should not lock the account.
- **Only changes are sent:** `SourceHealth{source_id, healthy, detail}`
  goes through the outbox, so it survives a reconnect. A provider failing
  over and over sends one event. After a restart, each source's first
  check is sent again. Providers configured on the agent itself are
  never reported.
- **`detail`** says why in words, at most 256 bytes and never containing
  credentials:
  - "the provider rejected the username or password (code 481)"
  - "host \"x\" not found"
  - "connection refused: check the host and port"
  - "timed out connecting: check the host, port and TLS setting"
  - "the TLS certificate is not valid: …"
  - "the provider refused the connection: too many connections are open
    for this account"

  A healthy report has an empty `detail`.

`TestSource{source_id}` (a query, for a "test connection" button) runs the
same check at once, within 30 seconds. It always tries a real login, even
after an earlier one failed, so a renewed account recovers from the button:

- `QueryResult{ok: true}` means it connected and logged in.
- `ok: false` puts the reason in `error`: the details above, or "unknown
  source", "source is disabled", or "source configuration cannot be
  unsealed".

A test that changes the state also sends a `SourceHealth` event.

Anyone holding the public key can seal, the cloud included, so the agent
treats sources as untrusted: public hosts and verified TLS only. A usenet
server on the user's LAN needs the host owner's consent on the agent
itself: its address in `network.allowed_ranges` (`NABBED_ALLOWED_RANGES`),
which the cloud cannot set, or the server configured locally
(`NABBED_NNTP`).
