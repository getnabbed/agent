# 0008: What the agent tells the cloud about itself

- **Status:** Proposed, 2026-10-29.
- **Scope:** The events and telemetry the agent sends so the web app can
  show what an agent and its downloads did. Today that is only in the
  agent's local log. The agent page in the web app is built by the cloud
  and is out of scope, except for what it needs from the agent.

## Problem

The web app's agent timeline shows connection events only: latest
heartbeat, connected, version, paired. The agent already sends every
download state change (`DownloadUpdate`), every import (`ImportCompleted`),
source health and library scans through the outbox, so most of the gap is
the cloud not showing them. But what's interesting about a download never
leaves the host:

| Only in the local log | Example |
|---|---|
| PAR2 outcome | "par2 verification passed", 4 files; "repairing with par2", 12 blocks |
| Extraction | "extracted archive", 4 volumes; "skipping archive entry" |
| Names | "named obfuscated file after the release" |
| Usenet | missing articles; "giving up on article"; provider fallback |
| Import | the placement method: rename, link or copy |
| Totals | bytes, time taken, average speed, which source |

Agent-level problems are local only too:
- no library folders allowed;
- an allowed folder that doesn't exist;
- an incomplete library scan;
- a provider allowing fewer connections than configured.

`AgentNotice` exists in the contract for this, but nothing sends it. Free
disk space is never checked at all: the contract has `HostStats` and
`SetAgentSettings.host_stats`, but neither is implemented.

## Goals

- **The timeline tells the story:** a user can see from the web app
  alone why a download took the path it did.
- **Agent problems surface:** they reach the user without anyone reading
  the host's log.
- **Same guarantees as today:** everything durable goes through the
  outbox, in the same transaction as the state it describes.
  Telemetry stays lossy.
- **No new privacy surface:**
  - no absolute host paths outside library roots;
  - no secrets;
  - nothing the cloud doesn't already know the subject of, such as file
    names inside a download or library.

Raw log shipping is a non-goal; see [Later](#later).

## Contract additions

All additive in `nabbed.agent.v2`, published in one BSR push.

```proto
enum Severity {
  SEVERITY_UNSPECIFIED = 0;
  SEVERITY_INFO = 1;
  SEVERITY_WARNING = 2;
  SEVERITY_ERROR = 3;
}

// Note is one thing worth showing that happened during a stage.
message Note {
  string code = 1;                  // stable, e.g. "par2.repaired"
  string message = 2;               // readable English, at most 256 bytes
  Severity severity = 3;
  map<string, string> attrs = 4;    // at most 8, for the UI to format
}

message DownloadStats {
  uint64 bytes = 1;                 // payload bytes transferred
  uint64 duration_ms = 2;           // the final download run
  uint64 avg_bps = 3;
  string source_id = 4;             // usenet source used, if any
  uint32 missing_articles = 5;
  uint32 peers = 6;                 // torrent: peers seen
}

message DownloadUpdate {
  // existing: download_id = 1, state = 2, failure = 3
  repeated Note notes = 4;          // what happened in the stage just left
  DownloadStats stats = 5;          // on leaving DOWNLOADING, and on DONE
}

message AgentNotice {
  // existing: code = 1, message = 2
  Severity severity = 3;
  map<string, string> attrs = 4;
  string subject_id = 5;            // root, source or download id, if any
  bool resolved = 6;                // the condition named by code+subject cleared
}

message RootDiskFree {
  // existing: root_id = 1, free_bytes = 2
  uint64 total_bytes = 3;
  bool downloads = 4;               // the downloads folder; root_id is empty
}
```

The cloud shows `message` as is, and may format by `code` and `attrs`
instead. Codes are listed in the tables below and documented with the
proto.

## Agent changes

### 1. Notes and stats on download updates

- **Collecting:** `engine.Job` gains a note collector, `job.Note(code,
  severity, message, attrs)`. The usenet and torrent engines, the
  assembler (PAR2, extract, names) and the importer call it where they
  log today; the log lines stay.
- **Sending:** the manager drains the collector into the `DownloadUpdate`
  of the transition that leaves the stage, in the same transaction.
- **Crashes:** a crash mid-stage loses that stage's notes, but the stage
  reruns from its checkpoint and notes again. Notes aren't persisted
  separately.
- **Bounds:** at most 20 notes per update, keeping the most severe; at most
  256 bytes per message and 8 attrs. Repeats are folded, for example 140
  "giving up on article" become one `usenet.articles_missing` note with
  `count=140`.
- **Stats:** filled from the engine's progress accounting and `Result`,
  and sent on leaving DOWNLOADING and again on DONE.

| Code | Stage | Severity | Attrs |
|---|---|---|---|
| `usenet.articles_missing` | downloading | warning | `count` |
| `usenet.provider_fallback` | downloading | info | `from`, `to` (source ids) |
| `par2.verified` | assembling | info | `files` |
| `par2.repaired` | assembling | info | `blocks`, `volumes_fetched` |
| `par2.repair_failed` | assembling | error | `missing_blocks` |
| `name.restored` | assembling | info | `from`, `to` |
| `name.from_release` | assembling | info | `from`, `to` |
| `extract.done` | assembling | info | `archive`, `volumes`, `kind` |
| `extract.entries_skipped` | assembling | warning | `count`, `reason` |
| `import.placed` | importing | info | `method` (rename, link, copy), `files` |
| `fetch.retried` | fetching | info | `attempts` |

### 2. Agent notices

- **Changes only:** a notice is sent when a condition starts, with
  `resolved=true` when it clears. Each code and subject pair keeps its
  state in a new `notices` table, so restarts don't repeat open notices.
  They're re-sent only if their state changed.
- **Rate limit:** at most 30 notices per hour.

| Code | When | Severity | Resolves |
|---|---|---|---|
| `agent.upgraded` | Start-up with a version other than the stored one | info | at once |
| `library.none_allowed` | Start-up with no allowed folders while the cloud has sent roots | warning | when one is allowed |
| `library.allowed_missing` | An allowed folder doesn't exist | warning | when it appears |
| `library.root_unusable` | A root fails the policy or is not writable at import or scan | error | on the next successful use |
| `library.scan_incomplete` | A scan's last report has `incomplete` | warning | on a complete scan |
| `disk.low` | Free space in the downloads folder or a root is under 5% or 10 GB | warning | when it's above 8% and 15 GB |
| `usenet.connections_capped` | A provider allows fewer connections than configured | info | when the cap goes away |

`SourceHealth` stays as it is; it already covers failing sources.

### 3. Free disk space

- **Telemetry:** implement `HostStats.disk_free` for each library root and
  the downloads folder. It's sent as lossy telemetry every 60 s while
  `SetAgentSettings.host_stats` is on, which means implementing
  `SetAgentSettings` too, for the host-stats flag only. Concurrency and
  bandwidth stay unsupported here.
- **Notices:** the `disk.low` notice works without telemetry, from the
  same check every 60 s.
- **Platform code:** split as `diskfree_unix.go` (`unix.Statfs`) and
  `diskfree_windows.go` (`GetDiskFreeSpaceEx`).

## Cloud side, for agreement

- **Timeline:** build the agent timeline from the outbox events, `CommandResult`s
  (applied or rejected, with the message) and connection events. Show
  notes under each download's state changes.
- **Version:** show the agent version as sent. It already starts with
  "v", so the page currently shows "vv0.0.0-…".

## Order of work

1. Contract additions, `make proto proto-lint proto-breaking`, BSR push,
   then the cloud pins the label.
2. Notes and stats on download updates: the biggest part of the gap.
3. Agent notices, with the `notices` table and the upgrade notice.
4. Free disk space: telemetry, `SetAgentSettings` and the `disk.low` notice.

Each step ships on its own. Agents and clouds that don't know the new fields
ignore them.

## Later

- **On-demand diagnostics:** a `Query` the cloud sends when a user asks
  for support, answered with the last few hundred log lines, redacted
  through the existing `LogValuer`s. It's opt-in per agent in its local
  config, so the host owner decides.
- **Torrent seeding stats** on the library entry: ratio and upload, as
  telemetry.
