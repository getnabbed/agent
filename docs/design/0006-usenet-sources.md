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
| `host` | DNS name or IP. Must not be local: private, loopback, link-local, CGNAT, `localhost`, `*.local`, `*.lan`, `*.internal`, `*.home.arpa` → `SOURCE_HOST_BLOCKED`. Checked again after DNS at every dial. |
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

## Cloud-side status

Confirmed by the cloud session on 2026-10-24:

- Sealing, raw `sealed_to_key`, per-agent rows (`agent_sources.agent_id`)
  and resending stored bytes on rehydration already match.
- Re-pairing updates the agent's `unseal_public_key`;
  `DownloadSource.needsReseal` lets the UI ask for credentials again.

Open items for the cloud:

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

Not yet in the agent: `TestSource` (a "test connection" button gets
`UNSUPPORTED`) and `SourceHealth` events (the cloud's `record_health` is
ready for them).

Anyone holding the public key can seal, the cloud included, so the agent
treats sources as untrusted: public hosts and verified TLS only. A usenet
server on the user's LAN must be configured locally (`NABBED_NNTP`).
