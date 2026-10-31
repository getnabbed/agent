# 0007: Library import

- **Status:** Accepted, 2026-10-24. Roots, policy, naming, placement,
  import and library scans are implemented. Upgrades (`replaces_path`),
  multi-episode files and season packs are not.
- **Scope:** How a finished download becomes files in the user's library,
  and the local policy that bounds where the control plane may write.

## Local policy: allowed roots

The control plane chooses library roots, but only inside folders the host
owner allowed in the agent's own configuration, which the cloud cannot
change:

```yaml
library:
  allowed_roots: [/media/movies, /media/tv]   # or NABBED_LIBRARY_ROOTS
  file_mode: "0644"
  dir_mode: "0755"
```

- **None are allowed by default.** Every `PutLibraryRoot` is then `REJECTED`
  with `ROOT_NOT_ALLOWED`, and the message says how to allow a folder. The
  web app shows it; this is the normal first-run state.
- **Checks on a root:** its path must exist, be a folder and be writable.
  It is resolved through symlinks, and each parent is compared with the
  allowed roots using `os.SameFile`, which handles symlinked mounts and
  case-insensitive file systems. The check is repeated whenever the root is
  used, since a link may have changed.
- **Permissions:** files are created `0644` and folders `0755`, because the
  media server may run as another user (it depends on the installation).
  Hard links get the same mode.
- **The `library.import` capability** is advertised only once a folder is
  allowed. The cloud re-sends the library configuration when an agent
  gains a capability, so allowing the first folder and restarting brings
  the user's roots in.

## Commands and queries

| Message | Agent behaviour |
|---|---|
| `PutLibraryRoot` | Validates and stores the root. The fixed parts of a template may not be absolute or contain `..` (`TEMPLATE_UNSAFE`). At most 64 roots. |
| `RemoveLibraryRoot` | `ROOT_IN_USE` while a download that isn't done or cancelled targets it. An unknown id succeeds, so removals replay. |
| `SetImportPolicy` | Stores the mode, sanitiser and extras. Before one arrives, the agent uses AUTO with no extras. |
| `BrowseFilesystem` | `QueryResult.payload` is JSON `{path, entries:[{name, path}], next_page_token}`. It lists folders only, 200 per page, and skips hidden names. With `path: ""` it lists the allowed roots; anything outside them is refused. |
| `RescanLibrary` | `APPLIED` once the root exists (else `UNKNOWN_ID`) and passes the policy check (else `ROOT_NOT_ALLOWED` or `INVALID_ARGUMENT`). The scan runs in the background and reports `InventoryReport` events; see [Library scans](#library-scans). |

## Naming

Paths are rendered exactly as the web preview renders them
(`packages/app/src/lib/naming.ts` in the cloud repository), checked against
its outputs in `internal/library/testdata/naming.json` and
`TestMatchesTheWebPreviewChecks`.

- **Tokens:** `{name}` or `{name:NN}`, case-insensitive; spaces count as
  underscores; `release_year` means `year`.
  - TV: `series_title`, `season`, `episode` (`episodes[0]`),
    `episode_title`, `year`, `quality`.
  - Movies: `movie_title`, `year`, `edition`, `quality`.
  - `release_group` is empty until NamingContext carries it.
  - Unknown tokens stay as written. Templates have no extension; the
    source file's is appended.
- **Padding:** `:NN` pads with zeros to the larger of its digit count and
  value (`:00` and `:02` give 2, `:3` gives 3), strings included.
- **`{quality}`:** `<Source>-<Resolution>`, with punctuation removed from
  the source (`WEBDL-1080p`), or whichever part is known.
- **Sanitising:** each token value has `<>:"/\|?*` replaced. With
  `ascii_only`, it is NFKD-normalised and stripped to printable ASCII.
- **Collapse, per segment:**
  - remove empty `()` `[]` `{}` pairs until none remain;
  - turn runs of spaces into one;
  - strip spaces and hyphens from both ends;
  - drop an empty folder segment.
  "Arrival ({year})" with no year becomes "Arrival". Year 0 is empty.
- **Specials:** for season 0 with a non-empty `specials_folder`, the folder
  segment that uses `{season}` becomes the specials folder.
- **Safety pass (agent only), per segment:**
  - control and format characters are removed;
  - trailing dots and surrounding spaces are trimmed;
  - reserved Windows names get a leading `_`;
  - each segment is at most 240 bytes, cut on a rune boundary;
  - an empty file name fails the import.

## Import

1. **Pick the files:** the largest video file in the assembled result, plus
   files whose extensions are in `extras` (literal extensions, normalised).
   Anything in a `sample` folder is skipped. Extras take the video's name.
   A second extra with the same extension becomes `.2.srt`.
2. **Render the path.** A TV download needs a season and an episode;
   otherwise the import fails `IMPORT`, not retryable.
3. **Place each file** inside the root. Folders are created and opened
   through `os.Root`, so no link leads out of the root. On Unix the file is
   then written, linked or renamed through the open destination folder,
   never by path, so a folder on the way that someone swaps for a link
   afterwards cannot redirect it. File modes are set through the open
   copy or the download's own path, never the library path.
   - **MOVE:** rename. Across file systems, copy, then delete the download.
   - **COPY:** write a partial copy, created exclusively under a hidden,
     random name (`.<name>.<8 hex>.nabbed-partial`), sync it, then rename
     it into place. Nothing planted under that name is followed. A rerun
     first removes partial copies of the same file left by a crash.
   - **HARDLINK:** a link, falling back to a copy when links fail. A name
     already taken is a collision, not a reason to copy.
   - **AUTO:** torrents are hard-linked when on the same file system and
     copied otherwise, so they keep seeding. Usenet downloads are moved.

   Existing files are never overwritten, including one that appears while
   the file is being placed, as from a concurrent import: the last step is
   a link, or a rename that refuses to replace (`renameat2` with
   `RENAME_NOREPLACE` on Linux, `renameatx_np` with `RENAME_EXCL` on macOS,
   `MoveFileEx` without `MOVEFILE_REPLACE_EXISTING` on Windows). Where the
   file system has no such rename (NFS, some FUSE and SMB mounts), a hard
   link and an unlink stand in for it. Only on file systems with neither
   (FAT, exFAT) is it a check followed by a rename. On Windows the copy is
   still written through the open folder, but the link or rename into it
   uses the folder's path, since Windows has no link or rename relative to
   an open folder. Directory junctions need no privileges there, so someone
   who can write to the library could still swap a folder on that path for
   a junction in the moment before the file lands. The one exception to
   never overwriting is a file at `replaces_path` with the same name, and
   the cloud doesn't send that yet. Nothing in the library is ever deleted.
4. **Record and report:** write `import.json` in the work directory, then
   return `ImportCompleted{download_id, root_id, files (relative to the
   root, extras included), quality}`. The manager queues it in the same
   transaction that moves the download to DONE.

**Reruns.** A rerun with `import.json` present reports the same files. A
rerun after a crash partway through treats a missing download with a file
already at its destination as placed.

**Failures:**

| Failure | Class | Retryable |
|---|---|---|
| Unknown root, or the policy or root changed | `IMPORT` | yes |
| No video, missing naming details, or the name is taken in the library | `IMPORT` | no |
| I/O errors | `DISK` | yes |

A `RetryDownload` of a download that failed while assembling or importing
resumes at that stage, with the files already on disk: the agent sends
`DownloadUpdate` IMPORTING (or ASSEMBLING) and nothing is downloaded again.
A download that failed earlier goes back to QUEUED.

## Library scans

A `RescanLibrary` reports the video files in one root, so the cloud knows
what the user already has.

- **What's reported:** files with a video extension (the list the importer
  uses), as `InventoryEntry{root_id, relative_path, size, mtime_ms}`. The
  path is relative to the root and slash-separated.
- **What's skipped:**
  - hidden names;
  - `sample` and `samples` folders;
  - unfinished `.nabbed-partial` copies;
  - the folder of any other configured root, so a TV root inside a movies
    root is reported only by its own scan.
- **Symlinks:** a symlinked file is followed only if its target is inside
  the allowed roots, and a symlinked folder is not followed.
- **Reports:** at most 500 entries each, queued through the outbox in
  order. Every report carries `root_id` and `scan_id` (the command's id),
  and the final one has `last` set. An empty root sends one `last` report
  with no entries. `incomplete` on the last report means some folders
  couldn't be read, or the root couldn't be scanned at all, so the cloud
  must not treat unreported files as deleted.
- **Restarts and repeats:**
  - A scan is recorded in `library_scans` with the command. A restart
    starts it over with the same `scan_id`, so entries may repeat within a
    scan; the cloud deduplicates by `(root_id, relative_path)`.
  - A new `RescanLibrary` for the same root cancels the running scan. Its
    reports stop, and the new scan begins once the old one has stopped.
  - Removing the root drops its scan.
- **Only on request:** the agent never scans by itself.

## Not yet

- **`replaces_path`:** delete versus trash for the old file is undecided.
  Until then nothing is deleted.
- **Multi-episode files, season packs and `SxxEyy` matching:** need a
  NamingContext field for the multi-episode style first. The cloud sends
  single episodes only.
- **A library-root inventory in `Hello`,** like `local_sources` (design
  0006), so roots reconcile without relying on the capability being newly
  gained.
- **Plex or Jellyfin scans** after import: neither side triggers one yet.
