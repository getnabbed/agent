# 0005: Usenet pipeline

- **Status:** Accepted, 2026-10-24. Connection pool and PAR2 verification
  implemented; repair and extraction are planned.
- **Scope:** How the agent turns an NZB into verified, extracted files:
  NNTP, yEnc, PAR2 and archive extraction. Import into the library is
  separate.

## Build or buy

The agent ships as one static binary with no cgo (CLAUDE.md), which rules
out the fastest usenet libraries. Each stage was checked in October 2026.

| Stage | Decision | Why |
|---|---|---|
| NNTP | Own code | The protocol is small; the value is in pooling and provider failover, which we want to control. `javi11/nntppool` decodes through `rapidyenc`, which needs cgo by default, and its API moved through v3–v5 in 2026. Our client already refuses plaintext credentials and bounds line length. |
| yEnc | Own code | `rapidyenc` without cgo needs `GOEXPERIMENT=simd` and speeds up amd64 only. `go-newsgroups/yenc` is in-memory only and new. A scalar decoder suffices for 1 Gbit/s; Go assembly (which needs no cgo) is the escape hatch if profiling says otherwise. |
| PAR2 | Own code | `go-newsgroups/par2` (v0.3, BSD-3) has not validated repair against par2cmdline output; `akalin/gopar` (BSD-3) has no releases. General Reed–Solomon libraries use a different field and matrix layout. We write it, test against par2cmdline fixtures and cross-check with both libraries. |
| RAR | `nwaples/rardecode/v2` ≥ 2.2.0 | Mature, pure Go, BSD-2; multi-volume, solid and encrypted archives; reads through a caller-supplied `fs.FS`. 2.2.0 fixes GO-2025-4020 (unbounded dictionary size). |
| 7z | `bodgit/sevenzip` | Mature, pure Go, BSD-3, multi-volume. |
| ZIP | standard library | |

## Stages

1. **Download (done).** Articles are fetched through a connection pool per
   provider and decoded into `data/` in the job's work directory.
2. **Verify and rename (done).** The PAR2 index is downloaded with the
   data; recovery volumes are deferred. Each protected file is matched by
   size and the MD5 of its first 16 KiB, so obfuscated names are found and
   renamed to the recovery set's names (sanitised like NZB names, never
   over an existing file). With no missing articles, that match is enough:
   every byte already passed a yEnc CRC or torrent piece hash. Otherwise
   each file is hashed per slice to count the blocks repair needs.
   The outcome the cloud sees:
   - All files verify: the download moves on, with the restored names.
   - Damage within the recovery blocks the release has (counted from the
     deferred volume names): `REPAIR`, retryable, until repair ships.
   - Damage beyond them: `INCOMPLETE`, not retryable, so the cloud picks
     another release.
   - No usable PAR2 and missing articles: `INCOMPLETE`, not retryable.
3. **Repair.** Only when verification finds damage: fetch just enough
   recovery volumes (deferred at download time) and repair over GF(2¹⁶).
4. **Extract.** RAR, 7z or ZIP, into the work directory only, with caps on
   dictionary size, total output and file count against archive bombs.
5. **Clean up.** Remove archives and PAR2 files, then hand the result to
   import.

An early availability check (`STAT` on a sample of articles before
downloading) waits for stage 2. It is only useful once the agent knows how
much damage PAR2 can repair, and that comes from parsing the index file.

## Connection pool

- **One pool per provider, shared by all downloads.** `connections=N` is
  the account limit, not a per-download count. Before this change, three
  downloads with eight connections each opened 24.
- **Route.** Each article is tried on the job's own source, then the other
  primary providers, then backups, in configuration order. A provider is
  skipped for an article when it answers 430 (not found) or serves a copy
  that fails its CRC or contradicts the NZB. Connection errors are retried
  up to three times on the same provider first.
- **Backups** (`backup=true`) are block accounts billed per byte. They are
  used only to fill gaps, never because a primary is down.
- **Failures.**
  - Bad credentials on a primary fail the download (not retryable). On a
    backup they disable that backup.
  - Five consecutive failed dials mark a provider down for a minute, with
    exponential back-off between attempts. The download fails, retryable,
    only when every primary is down.
  - A replaced or removed provider counts as down, so the download is
    retried with the new configuration instead of losing its articles.
- **Learned limit.** When a provider answers "too many connections" while
  the pool already has some open, the pool lowers its limit to what works
  and logs it once. Codes vary by provider (400, 481, 482, 502), so the
  message decides.
- **Idle connections** close after 60 seconds: providers count them against
  the account, which would block the user's other clients.

## Configuration

Until providers are configured in the web app and sealed to the agent
(`PutSource`), `NABBED_NNTP` takes space-separated provider URLs in
priority order:

```
nntps://user:pass@news.example.com:563?connections=20
nntps://user:pass@block.example.net:563?connections=8&backup=true
```

`Engine.SetProvider` and `RemoveProvider` are the hooks `PutSource` and
`RemoveSource` will call.
