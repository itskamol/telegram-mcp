# Spec: fast `send_file` that can outlive one tool call

Status: approved; measured (commit 1 ships mode C). Nothing implemented yet.

Companion of `docs/specs/download-media.md`, which did the same for
downloads; this reuses what it built (DC pools, the job registry, the
flood limit, the CLI over the hub socket) wherever it fits.

## Problem

`send_file` cannot send large files. Measured: a 30 MB file timed out at the
55 s `ToolTimeout` (under 0.55 MB/s), while downloads now reach ~5 MiB/s.
Causes, in `Service.SendFile` (`internal/tele/media.go`) and gotd's uploader
(v0.163.0, `telegram/uploader`):

1. **One request at a time.** `uploader.NewUploader(s.API)` has
   `threads: 1` (`uploader.go`). Files over 10 MB
   (`constant.UploadMaxSmallSize`) are "big" and could send parts in
   parallel (`bigLoop`), but only with `WithThreads(n)`.
2. **Small parts.** The part size starts at 128 KB and only doubles when the
   file would need more than 3999 parts (`computePartSize`, `part.go`). A
   150 MB file is sent as ~1200 parts of 128 KB; the maximum, 512 KB
   (`constant.UploadMaxPartSize`), needs ~300.
3. **One connection.** Every part goes over the primary connection, and
   Telegram limits the speed of each connection: for downloads, a pool of 8
   connections was 1.4–2.4× faster than one (download spec, 1.1).
4. **One tool call.** The whole upload and the send happen inside one call,
   bounded by the 55 s `ToolTimeout` and the clients' own ~60 s limit. A CLI
   wrapper alone does not help: it would call the same tool with the same
   limit.

Also: the uploader sleeps through any `FLOOD_WAIT` (`big.go`, `small.go`
call `tgerr.FloodWait` in a loop), as the downloader did.

The work lands as three commits, each with green `go vet ./...` and
`go test -race ./...`.

---

## Commit 1: parallel upload through the home pool

Inputs and outputs of `send_file` stay the same.

- **Where.** Uploads always go to the account's home DC. The parts are
  sent through the same home-DC pool downloads use (`dcCache`, keyed by the
  home DC, opened with `Pools.Pool(8)`), wrapped in `floodLimit`; if the pool
  cannot be opened, the primary connection is used, as for downloads.
- **Part size: 512 KB** (`WithPartSize(uploader.MaximumPartSize)`), for
  every file: 4× fewer requests than the default, and still within the
  3999-part limit up to ~2 GB, above `max_upload_mb`'s useful range.
- **Threads: 8** for big files (`WithThreads(downloadConcurrency)`), matching
  the pool: one request per pool connection at a time.
- **Small files (≤ 10 MB) stay sequential.** The protocol needs their MD5,
  computed in order (`uploadSmall`). With 512 KB parts that is at most 20
  requests; through a pool connection rather than the primary one, so a
  small upload does not queue behind other traffic.
- **Flood waits:** the same `floodLimit` as downloads: ≤ 60 s slept, longer
  fails fast with `FloodError`.
- **Progress:** `WithProgress` updates an atomic byte count (used by
  commit 2).
- **Measurement first, like downloads:** before this is written, an
  uncommitted build with a mode file compares, in Saved Messages only,
  A = today (1 thread, 128 KB, primary), B = 512 KB + 8 threads on the
  primary, C = B through the pool. Alternated (ABCABC…), 3 runs each, with a
  30 MB and a 150 MB random file. What ships is what measures best; if B is
  as fast as C, the pool is not used for uploads.

### Measurement result (2026-10-29, Saved Messages, server log)

Alternated runs, MiB/s; "cut" = stopped by the 55 s call timeout, nothing
sent:

| File | A (1 thread, 128 KB, primary) | B (8 threads, 512 KB, primary) | C (B through the home pool) |
| --- | --- | --- | --- |
| 30 MB | 0.38 cut, 0.57, 0.55 → **0.55** | 1.07, 2.17, 1.77 → **1.77** | 7.66, 7.94, 7.21 → **7.66** (3.6–4.0 s) |
| 150 MB | — | 2.03 cut, 2.02 cut | 10.58, 10.49 (13.5 s each) |

C is ~14× A and ~4× B; B alone does not get 150 MB through one call. No
session error appeared. **C ships**: the pool, 512 KB parts, 8 threads.

## Commit 2: background mode for `send_file`

Same shape as `download_media`'s (download spec, 2.1–2.5), reusing its
registry.

- **Behaviour.** `send_file` starts (or finds) a job and waits ~45 s. Done
  in time → today's answer plus `status: "sent"`. Not done →
  `{status: "uploading", uploaded, size, eta_seconds, next}`; the job goes on
  in the server. Failed → the error, reported once, then the next call tries
  again.
- **Key.** `(chat, idempotency_key)` when a key is given. Without one, a key
  is derived from what makes the message: the file's real path, size and
  modification time, caption, parse_mode, as_document, reply_to and
  topic_id. A call repeated while the job runs, or within the 30-min result
  TTL, gets the same job and never sends a second message.
  - *Change:* today two identical calls without a key send twice. With this
    rule, the second identical call within 30 minutes returns the first
    message instead. The tool description says: to send the same file again
    on purpose, pass a new `idempotency_key`.
  - The derived key is used only by the registry; `random_id` stays random
    without an explicit key, so a later identical send (after the TTL) is
    never silently swallowed by Telegram.
- **The file is checked once and held.** The first call runs
  `fsguard.OpenFile` (allowed_roots, no hidden paths, symlinks resolved,
  checked == opened) and the job keeps that open `*os.File` until it ends;
  it never reopens the path. Later calls still run `OpenFile` first, so a
  path that has become forbidden is refused before any job lookup.
- **Registry.** The `Downloads` registry is generalised to jobs of two kinds
  (download, upload), each with its own limit of 2 running jobs, so large
  uploads cannot block downloads. Shutdown, the 1 h cap, the stall watchdog
  (no bytes for 2 min) and the 10 s shutdown limit apply as for downloads.
  Nothing is written locally, so there is no crash sweep; parts already sent
  to Telegram expire there on their own.
- **Where a send can be interrupted.** Cancelled while uploading → nothing
  is sent, the audit line has the error. Cancelled after `messages.sendMedia`
  went out → the message may have been sent; the answer says so, and a
  retry with an `idempotency_key` is safe (as today, `TimeoutAdvice`).
- **Audit.** One `send_file` line per job, when it ends (with the message
  id, or the error), as `sent` writes it today. Polls are not audited.

## Commit 3: `telegram-mcp send` CLI and descriptions

```bash
telegram-mcp send --chat me --file report.pdf [--caption "…"] [--parse-mode markdown]
                  [--as-document] [--reply-to N] [--topic-id N] [--idempotency-key K]
```

- Calls `send_file` through the running server until `status: "sent"`;
  prints the message id on stdout, progress on stderr, exit 1 on failure.
  No Telegram connection of its own (`hub.Connect`).
- **Full socket, not read-only:** `send_file` is a write and exists only on
  the full server. With `read_only` the tool is absent and the command
  fails with a clear message; it cannot get around `read_only`.
- **Always idempotent:** without `--idempotency-key` the CLI makes a random
  one and prints it on stderr before the first call, so the loop's repeated
  calls are one job, and a re-run after a crash can pass the same key.
- `send_file`'s description and its `uploading` answer (`next`) point
  shell-capable agents at the CLI, as `download_media` does.

---

## Tests

Commit 1 (`internal/tele`, fake pools and a fake DC that records
`upload.saveBigFilePart` / `saveFilePart`):
- A big file goes through the home pool, 8 parts in flight at most, 512 KB
  parts, every part exactly once; the primary connection gets no part.
- A small file goes through the pool sequentially with 512 KB parts.
- Pool open fails → upload over the primary connection; next one retries.
- `FLOOD_WAIT_3` retried, `FLOOD_WAIT_120` → `FloodError` at once.
- Progress counts reach the file size.

Commit 2 (`internal/mcpserver`, gated fake upload):
- First call `uploading`; second identical call (no key) is the same job,
  nothing sent twice; done → `sent` with the message id; one audit line.
- With an explicit key: same; with a different key: a second job.
- After done, an identical call within the TTL returns the first message;
  after the TTL, it sends again with a fresh `random_id`.
- Failure reported once, then retried; upload/download limits are separate.
- The job uses the file handle opened by the first call: replacing the file
  at the path afterwards does not change what is sent.
- A path made forbidden after the first call is refused on the next call.
- Shutdown cancels an upload: nothing sent, one audit line with the error.

Commit 3 (`internal/dlclient` generalised, or a sibling `sendclient`):
- Loop until sent; error at once; cancel stops waiting only.
- The CLI passes the same generated key on every call.
- Read-only server: clear error, nothing sent.

Mutation checks as for downloads.

## README "Safety settings"

- `send_file`: a send that takes longer than ~45 s goes on in the server
  (at most 2 at a time, 1 h each, ended after 2 min without progress);
  calling again returns progress or the message. An identical call within
  30 minutes, or one with the same `idempotency_key`, never sends twice.
  The file is checked and opened once; the server sends what it opened.
- `telegram-mcp send`: uses the running server's full socket; refused under
  `read_only`.

## Measurement plan (Saved Messages only)

1. Save `config.toml`; set `max_upload_mb = 200` temporarily.
2. Make `bench-up-30mb.bin` (30 MB, not a multiple of 1 MiB) and
   `bench-up-150mb.bin` in the first allowed root from `/dev/urandom`.
3. Builds A/B/C as in commit 1, alternated, 3 sends each per file, timed
   from the server log; record every message id.
4. Then the background mode with the 150 MB file, through the tool and
   through `telegram-mcp send`.
5. Cleanup always runs: `delete_messages` in `me` with exactly the recorded
   ids, remove the local files, restore `config.toml` and check the diff is
   empty, restore the binary.

## Not doing

- No resumable uploads across restarts (gotd's `Upload` could resume within
  a process; a restart starts over).
- No change to `max_upload_mb`'s default.
- No upload to other DCs: files are always uploaded to the home DC.

## Review decisions

1. Identical calls without a key are one job for 30 minutes (commit 2,
   "Change"); a new `idempotency_key` sends the same file again on purpose.
2. Separate limits: 2 uploads and 2 downloads at once.
3. Measure first (commit 1). Mode A cannot finish even 30 MB inside one
   call, so runs are compared by bytes per second, which the server logs
   whether the upload finishes or is cut at the call's timeout; a cut
   upload never reaches `messages.sendMedia`, so nothing is sent.
