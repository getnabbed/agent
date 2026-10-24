# 0006: Usenet sources from the control plane

- **Status:** Implemented in the agent, 2026-10-24. Cloud side to match.
- **Scope:** The contract for `PutSource` and `RemoveSource` with
  `kind = "nntp"`: how the web app seals provider credentials, what the
  agent checks, and what the cloud must handle.

## Contract

### Sealing (web app)

The browser seals the configuration with libsodium `crypto_box_seal` to the
agent's X25519 public key. That key is `public_keys.x25519` from
`POST /api/device/authorize`: base64url **without padding**, 32 bytes. With
`libsodium-wrappers`, decode it with
`sodium.from_base64(key, sodium.base64_variants.URLSAFE_NO_PADDING)`; the
default variant is standard base64 and fails or mis-decodes.

The cloud stores the sealed bytes and cannot open them. It must forward the
**same bytes** on every `PutSource` for that source (including rehydration).
Re-sealing produces new bytes, which the agent treats as a changed source and
reconnects the provider's pool for.

The agent's test `TestOpensLibsodiumSealedBox` opens a box made by
libsodium 1.0.20; the web app's output must open the same way.

### `Source` fields

| Field | Rule |
|---|---|
| `id` | 1–64 of `A-Z a-z 0-9 _ -`. UUIDs fit. |
| `kind` | `"nntp"`. Anything else: `SOURCE_KIND_UNSUPPORTED`. |
| `display_name` | Up to 128 bytes; used in logs only. |
| `sealed_config` | The sealed box, 1 byte–16 KiB. |
| `sealed_to_key` | The raw 32-byte X25519 public key it was sealed to (not base64, not a thumbprint). If it differs from the agent's key: `SOURCE_UNSEALABLE` with a "sealed to a different agent key" message. |
| `enabled` | `false` keeps the source stored but unused. |

### Sealed plaintext, version 1

UTF-8 JSON, one object. Unknown fields are rejected, so a misspelling fails
at `PutSource` instead of as an authentication failure mid-download.

```json
{
  "v": 1,
  "host": "news.newshosting.com",
  "port": 563,
  "tls": true,
  "username": "user",
  "password": "secret",
  "connections": 30,
  "backup": false
}
```

| Field | Rule |
|---|---|
| `v` | Must be `1`. |
| `host` | DNS name or IP. Must not be local: private, loopback, link-local, CGNAT, `localhost`, `*.local`, `*.lan`, `*.internal`, `*.home.arpa` → `SOURCE_HOST_BLOCKED`. Checked again after DNS at every dial. |
| `port` | 1–65535, default 563. |
| `tls` | Must be `true`. |
| `username`, `password` | Both or neither; no line breaks. |
| `connections` | 1–100, default 8. The account limit, shared by all downloads. The agent lowers it if the provider refuses connections. |
| `backup` | `true` for block accounts: used only for articles other providers lack or serve damaged. |

### Results

| Outcome | When |
|---|---|
| `APPLIED` | Stored; the provider is live after commit. Also for `RemoveSource` of an unknown id, so removals can be replayed. |
| `REJECTED` + `SOURCE_KIND_UNSUPPORTED` | `kind` is not `nntp`. |
| `REJECTED` + `SOURCE_UNSEALABLE` | Wrong key, tampered or truncated box. |
| `REJECTED` + `SOURCE_HOST_BLOCKED` | Local host. |
| `REJECTED` + `INVALID_ARGUMENT` | A field is missing or out of range; the message names it. |
| `REJECTED` + `LIMIT_EXCEEDED` | More than 32 sources. |

Result messages never contain secrets and are safe to show users.

### Capabilities

Capabilities are only sent in `Hello`. When a command changes them (the
first enabled source adds `download.usenet`; removing the last takes it
away), the agent sends the `CommandResult` and then closes the stream and
reconnects at once with a new `Hello`. Expect one immediate reconnect after
such a command. It is not an error and should not count as flapping.

Send usenet `StartDownload` commands only while the latest `Hello` lists
`download.usenet`. `StartDownload.source_id` names the preferred source;
the agent falls back to its other providers for missing articles.

## Issues for the cloud side

1. **Re-pairing invalidates every source.** A new pairing has a new X25519
   key, and nobody can re-seal the old blobs: the cloud cannot read them
   and the browser no longer has the password. Every `PutSource` after a
   re-pair (and the rehydration of a reinstalled agent) returns
   `SOURCE_UNSEALABLE`. The web app should mark those sources "re-enter
   credentials" when it sees that result, or when an agent is re-paired.
2. **Sealing is per agent.** A tenant with two agents needs one sealed copy
   per agent. Store `sealed_config` per (source, agent), not per source.
3. **No priority field.** The agent tries primaries in the order sources
   were first added. Proposal: add `int32 priority` to `Source`.
4. **`backup` is inside the sealed blob,** so the cloud cannot show it.
   It is not secret. Proposal: a plain `role` field (`PRIMARY`, `BACKUP`)
   on `Source`; the agent would then ignore `backup` in the blob.
5. **Anyone holding the public key can seal**, the cloud included. The agent
   therefore treats a source as untrusted: public hosts only and TLS
   required. Users with a usenet server on their LAN must configure it
   locally (`NABBED_NNTP`), not in the web app.
6. **Do not reseal on rehydration;** resend the stored bytes (see Sealing).
7. **Never send the plaintext** to cloud-api: seal in the browser before
   the GraphQL mutation, and keep the password out of client-side error
   reporting.
8. **Not implemented yet in the agent:** `TestSource` (a "test connection"
   button will get "unsupported") and `SourceHealth` events (no per-source
   status in the UI yet). Download failures still name the provider.
9. **A capability change reconnects the stream.** Presence may blip. A
   later protocol version could add an in-band capabilities update.
