# 0005: Usenet pipeline

- **Status:** Accepted, 2026-10-24. Connection pool, PAR2 verification
  and repair, extraction and clean-up implemented.
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
| PAR2 | Own code | `go-newsgroups/par2` (v0.3, BSD-3) has not validated repair against par2cmdline output; `akalin/gopar` (BSD-3) has no releases. General Reed–Solomon libraries use a different field and matrix layout. We wrote it and test it against par2cmdline fixtures: our encoder reproduces its recovery slices byte for byte, and it verifies our repairs. |
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
   each file is hashed per slice to find the slices repair must rebuild.
   An obfuscated file whose first 16 KiB is damaged is found by its exact
   size and any slice that still matches. A file with the right content
   and the wrong length only needs truncating. All files verifying moves
   the download on, with the restored names; no usable PAR2 and missing
   articles fails `INCOMPLETE`, not retryable.
3. **Repair (done).** Only when verification finds damage.
   - **Fetching volumes.** Volumes already in the work directory are read
     first. While blocks are short, the assembler asks the engine for
     more of the deferred volumes: the set whose blocks (from the
     `.volNN+MM` names) cover the shortfall with the fewest blocks,
     smaller volumes first on ties. A volume that arrives with missing
     articles yields fewer blocks, so another round may follow. The usenet
     engine implements this as `FetchFiles`, through the shared pools, yEnc
     decoding and `os.Root`, with its own checkpoint so a fetch resumes.
   - **Decoding.** Reed–Solomon over GF(2¹⁶) as PAR 2.0 specifies
     (polynomial 0x1100B; input slice i has constant 2^nᵢ, nᵢ the i-th
     logarithm not divisible by 3, 5, 17 or 257; 16-bit little-endian
     words). The matrix of lost slices against recovery exponents is
     inverted by in-place Gauss–Jordan elimination, which also skips
     recovery slices that are not independent. Slices are streamed from
     disk in passes; buffers and matrix stay within 256 MiB, so a large
     repair takes more passes rather than more memory. A test-only
     encoder reproduces par2cmdline's recovery slices byte for byte, and
     par2cmdline verifies our repaired fixtures.
   - **Writing.** Only rebuilt slices are written, in place, through
     `os.Root`. Missing files are created under their recovery-set names
     and never over a file that appeared since verification. Each file is
     then truncated to its declared size and must match its MD5. Since
     every byte written is correct, a repair interrupted by a crash is
     finished by the next run, which verifies again from the stored
     engine result. Repaired output then goes through extraction and
     clean-up like a release that verified.
   - **Failures.**
     - More damage than the release's blocks, recovery slices that cannot
       solve it, or a repair too large: `INCOMPLETE`, not retryable, so
       the cloud picks another release.
     - A repaired file that fails its MD5 (the recovery data is wrong):
       `REPAIR`, not retryable.
     - Write errors: `DISK`, retryable. A failed fetch keeps the engine's
       class (for example `CLIENT` while every provider is down).
     - Without a fetcher, repairable damage fails `REPAIR`, retryable.
   - **Limits.** Blocks are found only at their own offsets in their own
     file: data shifted within a file, or one file's data under another's
     name, is not searched for as par2cmdline does with `-N`. Decoding is
     scalar and single-threaded.
4. **Extract (done).** RAR (single and multi-volume, `.partN.rar` and
   `.rNN` naming), 7z (including `.7z.001` splits) and ZIP, into
   `extracted/` in the work directory. Torrents go through the same step.
   - **Finding archives.** By name: the first volume of each set, confirmed
     by its signature. A file whose contents don't match its extension is
     left alone. Archives inside archives are not extracted.
   - **Paths.** Every entry is written through `os.Root`. Each path
     component is sanitised like an NZB name, `..` entries are skipped, and
     names that collide (including by case only) get a numbered suffix.
     Links and other non-regular entries are never created.
   - **Limits against bombs.**
     - Output may not exceed twice the downloaded size plus 1 GiB.
     - At most 10,000 entries per job.
     - Decoders may use at most 1 GiB of dictionary or model memory. RAR
       passes this to rardecode. For 7z the agent replaces the library's
       LZMA, LZMA2 and PPMd decoders with ones that check first, because the
       library allocates whatever the archive declares, up to 4 GiB. Zstd,
       Brotli and LZ4, which only third-party 7-Zip builds write, are
       refused.
   - **Failures.** Password protection, missing volumes, bad checksums and
     exceeded limits fail `EXTRACT` and are not retryable, so the cloud
     picks another release. Write errors fail `DISK` and are retryable.
5. **Clean up (done).** After a successful extraction, the archive volumes
   and PAR2 files are deleted. The result handed to import lists the
   extracted files plus any downloaded files that were neither archives
   nor PAR2 (an `.nfo`, a sample). `extract.json` in the work directory
   records the outcome before anything is deleted, so a restart during
   clean-up finishes it instead of looking for archives that are gone.

Extraction needs free space for a second copy of the release until clean-up
runs. Deleting each RAR volume as soon as it has been read would avoid
that, but it would make a failed extraction impossible to retry.

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

`PutSource` and `RemoveSource` (design 0006) call `Engine.SetProvider` and
`RemoveProvider`.
