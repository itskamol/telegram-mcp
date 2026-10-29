# Spec: fast `download_media` that can outlive one tool call

Status: draft, waiting for review. Nothing here is implemented yet.

## Problem

`download_media` hits the 55 s `ToolTimeout` on large files, even after
`max_download_mb` is raised. Two causes, both in `Service.Download`
(`internal/tele/media.go`):

1. `downloader.NewDownloader().Download(s.API, loc).Stream(ctx, w)` fetches
   512 KB parts **one at a time**, waiting a full round trip for each.
2. Every request goes to the **home DC** through `c.API()`. A file stored on
   another DC is refused with `FILE_MIGRATE_X`; gotd's `invokeDirect`
   (`telegram/invoke.go`) then repeats the request on a sub-connection
   (`invokeSub`, `telegram/sub_conns.go`). So each part of such a file costs
   two round trips, one of them a guaranteed failure.

A 200 MB file is 400 sequential parts, i.e. 400–800 round trips. At
~100–150 ms each, that is 40–120 s before any bandwidth limit.

Even after the fix, a big enough file on a slow link will take longer than one
tool call, so the tool also needs a way to finish in the background.

iyear/tdl downloads fast with the same gotd primitives (1 MB parts,
`WithThreads`, a pool per DC). tdl is AGPL-3.0: **no code is copied from it**,
only those publicly documented ideas, all of which are gotd APIs.

The work lands as two commits, each with green `go vet ./...` and
`go test -race ./...`.

---

## Commit 1: parallel download on the file's DC

The tool's inputs and outputs do not change.

### 1.1 Where the requests go

`Service.Media` already sees the `*tg.Document` / `*tg.Photo`; both carry
`DCID`. Store it in `MediaFile` (new field `DC int`).

`Service` gets one new dependency, kept as a small interface so tests can fake it:

```go
// Pools opens connections to a given DC; *telegram.Client satisfies it.
type Pools interface {
	DC(ctx context.Context, dc int, max int64) (telegram.CloseInvoker, error)
}
```

`Service` keeps a `map[int]tg.Invoker` of opened DC pools behind a mutex,
created on first use and closed when `c.Run` returns (one `Service.Close`,
called from `runServe`). `c.DC` exports and imports the authorization on first
use (`auth.exportAuthorization` / `auth.importAuthorization`), so caching the
pool avoids that handshake on every download.

Routing:

| File DC | Invoker | Why |
| --- | --- | --- |
| home DC | `s.API` (the existing connection) | MTProto pipelines many in-flight requests on one connection, so threads still overlap. No extra connection on the primary auth key, which keeps us clear of anything that could look like `AUTH_KEY_DUPLICATED`. |
| other DC | cached `Pools.DC(ctx, dc, 4)`, max 4 connections | Requests go straight to the DC that has the file: no `FILE_MIGRATE`, one round trip per part. The pool uses the DC's exported key, not the primary one. |
| `DC == 0` (unknown) | `s.API` | Same as today; gotd's migrate handling still works. |

If measurement (below) shows the home DC is the bottleneck, `c.Pool(n)` for the
home DC is a later, separate change, tried live in Saved Messages first,
because those connections share the primary auth key.

This all runs in the **primary** process only. Relays (`internal/hub`) never
open Telegram connections, so the one-process-per-profile rule holds.

### 1.2 Part size and threads

```go
downloader.NewDownloader().WithPartSize(1 << 20).
	Download(inv, loc).WithThreads(downloadThreads).Parallel(ctx, w)
```

- **Part size 1 MB.** That is the maximum `upload.getFile` allows (limit
  must divide 1 MB and a part must not cross a 1 MB boundary; 1 MB parts at
  1 MB offsets always qualify). It halves the request count against 512 KB.
- **Threads: 8.** Reasoning:
  - 8 × 1 MB in flight is enough to fill a typical link at 100–150 ms RTT
    (≈ 50–80 MB/s ceiling), so the link, not latency, becomes the limit.
  - Memory stays bounded: 8 parts being fetched + gotd's write channel of 8
    = at most ~16 MB per download.
  - Official clients keep a similar number of requests per file in flight;
    16+ buys little and raises the chance of `FLOOD_WAIT`.
  - 8 threads over 4 pool connections = 2 in flight per connection.
  - The measurement plan sweeps 4 / 8 / 16 and the constant is changed if 4
    or 16 is clearly better. It stays a constant, not a config option.

### 1.3 Writing parts out of order: `fsguard.WriteNewAt`

`Parallel` needs an `io.WriterAt`; `fsguard.WriteNew` takes an `io.Writer`.
Add a sibling and keep `WriteNew` for other callers:

```go
// WriteNewAt is WriteNew for writers that fill the file out of order.
// size is the expected length (0 = unknown); a size above limit fails
// before anything is created.
func WriteNewAt(dir, name string, size, limit int64, write func(io.WriterAt) error) (string, error)
```

Every guarantee of `WriteNew` is kept:

| Guarantee | How |
| --- | --- |
| Inside `allowed_roots`, symlinks resolved | Unchanged: `dir` still comes from `ResolveDir`, which is called before any download. |
| Never overwrite | Unchanged: data goes to `os.CreateTemp(dir, ".telegram-mcp-*.part")`, then `os.Link` to the first free `name`, `name (1)`, …; `Link` fails on an existing file. The temp file is removed in every case. Shared code with `WriteNew` moves into one helper. |
| Size checked **before** | `size > limit` → `ErrTooLarge` before the temp file exists (the handler also checks `f.Info.Size` as today). |
| Size checked **during** | `limitWriterAt.WriteAt(p, off)` refuses `off < 0` or `off+len(p) > limit` with `ErrTooLarge`. Out-of-order writes mean a running total is wrong, so the check is on the end offset, which bounds the file size no matter the order. |
| Size checked **after** | When `size > 0`, the temp file's final length must equal `size`, else an error and nothing is kept. This catches a short download that `Parallel` thinks is complete. |
| `.part` then rename | Unchanged (link + remove, as above). |
| Concurrency | `*os.File.WriteAt` is safe for concurrent use (`pwrite`); gotd also serialises through `syncio.NewWriterAt`. The wrapper keeps no shared state except an atomic `written` counter (used in commit 2 for progress). |

### 1.4 Flood waits

The DC pool from `c.DC` is a raw invoker: the `floodwait.Waiter` middleware
installed on the client does **not** wrap it. And gotd's downloader retries
`FLOOD_WAIT` internally (`retryRequest`, `downloader/retry.go`), sleeping for
however long Telegram asks, which could be minutes.

So the pooled invoker is wrapped in a small `floodLimit` invoker:

- `FLOOD_WAIT` / `FLOOD_PREMIUM_WAIT` of **≤ 60 s** (same limit as the
  client's waiter) is passed through unchanged; gotd sleeps and retries.
- **> 60 s** is turned into a `*tele.FloodError{Seconds}` that is *not* a
  `tgerr.Error`, so gotd does not sleep; the download fails and `Explain`
  says "Telegram rate limit: wait N seconds".
- The home-DC path already goes through the waiter (same behaviour) and the
  same wrapper is applied there too, so both paths are identical.

Every sleep is still bounded by the context (tool call or, in commit 2, job).

### 1.5 `FILE_REFERENCE_EXPIRED`

File references expire (hours, sometimes sooner). On
`FILE_REFERENCE_EXPIRED` the download is retried **once** from the start:
the message is fetched again with `Service.Media` (fresh `FileReference`),
the same temp file is truncated and refilled. gotd's `Parallel` cannot
resume at an offset, and a mid-download expiry is rare, so a restart is
simpler than a resumable reader. A second expiry fails with the existing
hint ("the file reference expired; fetch the message again and retry").

This means `Service.Download` takes the chat + message (or a refetch
callback) instead of a bare location; the new signature:

```go
// DownloadFile writes f to w in parallel on f's DC. refetch reloads the
// message when the file reference expired.
func (s *Service) DownloadFile(ctx context.Context, f MediaFile, w io.WriterAt,
	refetch func(context.Context) (MediaFile, error)) error
```

### 1.6 `view_photo`

`view_photo` reads at most 5 MB into a `LimitedBuffer` (an `io.Writer`).
Decision: it **keeps the sequential `Stream`** (≤ 5 parts at 1 MB; parallel
buys little and would need an in-memory `WriterAt`), but it **does get the
file-DC routing and 1 MB parts**, which removes the `FILE_MIGRATE` double
round trip. So `Service.Download(ctx, loc, w)` stays, with a `dc` argument
added, and both paths share the invoker selection and flood wrapper.

### 1.7 Tests for commit 1

`internal/fsguard`:
- `WriteNewAt` with parts written in reverse / shuffled order from several
  goroutines (`-race`) produces the exact bytes.
- A write ending past `limit` → `ErrTooLarge`, no final file, no `.part`
  left in `dir`.
- `size` above `limit` → `ErrTooLarge` before any file is created.
- Final length ≠ `size` → error, nothing left behind.
- Existing `name` → result is `name (1)`; the existing file is untouched.
- Existing tests for `WriteNew` keep passing (shared helper).

`internal/tele` (fake `tg.Invoker` as in `tele_test.go`, fake `Pools`):
- A document with `DCID` ≠ home goes to `Pools.DC(…, dc, 4)`, never to
  `s.API`; with `DCID` = home it goes to `s.API`; the pool is opened once for
  two downloads and closed by `Service.Close`.
- Requests use `limit = 1 MB` and offsets that are multiples of 1 MB.
- `FLOOD_WAIT_3` is retried; `FLOOD_WAIT_120` fails fast with a rate-limit
  message (no sleep: test runs with a short context).
- `FILE_REFERENCE_EXPIRED` → refetch called once, download succeeds; twice →
  error with the existing hint.

`internal/mcpserver`: existing `download_media` / `view_photo` tests still
pass with the fakes; `view_photo` of a photo on another DC uses the pool.

---

## Commit 2: background mode, no new tool

### 2.1 Behaviour

`download_media` starts (or finds) a **job** and waits for it for at most
**45 s** — precisely `min(45 s, time left on the call's context − 5 s)`, so
resolving the chat and fetching the message never push it past the 55 s
`ToolTimeout`.

- Done in time → the same result as today, plus `status: "done"`:
  `{status:"done", path, size, mime_type}`.
- Not done → `{status:"downloading", downloaded, size, path}` and the job
  keeps running inside the server process.
- Failed → the tool returns the error (as today).

Calling again with the same chat + message returns the current progress or
the finished result and **never starts a second copy**. The tool description
says so: "Large files may return status=downloading; call again with the same
chat and message_id to get progress or the final path."

Output type change (additive, existing fields keep their meaning):

```go
type downloadOut struct {
	Status     string `json:"status"`               // "done" or "downloading"
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded,omitempty"` // bytes so far, while downloading
	MimeType   string `json:"mime_type,omitempty"`
}
```

While downloading, `path` is the **planned** path (`dir/name`); if that name is
taken when the file is finished, the final answer has `name (1)` and that is
the authoritative one. (Reserving the final name up front would leave a
half-written file under the real name, which `.part` exists to avoid.)

### 2.2 Job key and registry

A `downloads` registry lives on `mcpserver.Server` (primary process only).

- **Key:** `(chat_id marked, message_id)`. `dir` and `file_name` are not part
  of the key: a second call that asks for another dir or name while the job
  runs gets the running job's status, and a `note` field says the first
  call's dir and name are used. The account is implicit (one server per
  profile).
- The handler still runs the normal checks on every call **before** looking
  up the key: allowlist (`s.chat`), `ResolveDir`, message fetch, size limit.
  A repeat call cannot read a job it would not be allowed to start.
- **Finished jobs** (done or failed) stay in the registry for **30 min** so a
  repeat call gets the result; after that, or beyond 32 finished entries
  (oldest first), they are forgotten and a new call downloads again (into
  `name (1)`, as today).
- **Concurrency:** at most **2 running jobs** (each 8 threads). A third
  distinct download is refused: "2 downloads are already running; try again
  when one finishes" — honest and simple, no hidden queue.
- **Limits per job:** context with a 1 h cap, and a stall watchdog: no new
  bytes for 2 min → the job fails ("download stalled").

### 2.3 Cancel and restart

- **Client cancels the tool call** (MCP `notifications/cancelled`, or the 55 s
  timeout): only the *wait* ends. The job runs on the server's context, not
  the call's, and continues; the next call sees it.
- **Server stops cleanly** (stdin closed, SIGINT/SIGTERM): `runServe`'s
  context is cancelled, every job is cancelled, its `.part` is removed by
  `WriteNewAt`'s cleanup, and an audit line with
  `error: "canceled: server stopped"` is written for each.
- **Server killed** (`kill -9`, crash, power loss): the deferred cleanup
  never runs. To not leave `.part` files behind, each job records its temp
  path in a new `partial_downloads(path TEXT PRIMARY KEY, started INTEGER)`
  table in the existing database before writing, and deletes the row when
  the temp file is gone. At startup the primary removes every listed file
  **only if** its base name matches `.telegram-mcp-*.part` and it still
  resolves inside `allowed_roots`, then clears the table. Nothing else on
  disk is ever touched.
- After a restart the job is gone; the next call with the same chat+message
  simply starts a new download.

### 2.4 Relays (`internal/hub`)

Relays only pipe stdio to the primary's socket; every MCP session runs inside
the primary, so the registry and the downloads live there.

- Two sessions (e.g. Claude Desktop chat and a Code session) asking for the
  same message share one job.
- A relay process exiting does not affect the job.
- If the primary exits, its relays lose their connection anyway; jobs end as
  in "Server stops cleanly".
- Paths and limits are the primary's config (already true today).

### 2.5 Audit log

One `download_media` line **per job, when it ends**, written by the job, not
by the tool calls that poll it (polls are reads):

```json
{"tool":"download_media","chat_id":…,"message_ids":[…],"path":"/…/file.mp4"}
{"tool":"download_media","chat_id":…,"message_ids":[…],"error":"canceled: server stopped"}
```

`audit.Entry` does not change. A download that finishes inside the first call
produces exactly the same line as today. A `kill -9` produces no line, and
the startup sweep removes its partial file, so nothing written is unaudited.

### 2.6 Tests for commit 2

`internal/mcpserver` (fake download that blocks on a channel; the 45 s wait
is a field so tests use ~50 ms):
- First call returns `downloading` with `downloaded`/`size`; the fake has been
  started once.
- A second call with the same chat+message while running returns progress
  and does **not** start the fake again (call counter = 1); with a different
  `file_name` it also returns the same job plus `note`.
- After release, the next call returns `done` with the final path; one audit
  line in total.
- A failing job: the next call returns the error; one audit line with
  `error`.
- Cancelling the call's context leaves the job running.
- Cancelling the server context cancels the job, removes the `.part`, writes
  the `canceled` audit line.
- A third concurrent job is refused; finished entries expire after the TTL
  (injected clock).
- Stall watchdog fires with a short injected timeout.

`internal/storage`: `partial_downloads` add / remove / list.

Startup sweep: removes a listed `.telegram-mcp-x.part` inside a root; keeps a
listed path outside the roots and a listed path with another name.

`internal/hub`: two relayed clients request the same message → one download
(extend the existing `hub_test.go` setup).

---

## README "Safety settings" changes

Commit 1: none (same guarantees, same limits).

Commit 2, replace the `download_media` bullet and the timeout bullet:

- `download_media` writes only inside `allowed_roots` (symlinks resolved, `..`
  rejected), reduces file names to one safe component, never overwrites an
  existing file, and checks the size before, during and after the download.
  Data goes to a hidden `.telegram-mcp-*.part` file next to the target and
  becomes visible only when complete.
- A download that does not finish within about 45 s continues inside the
  server (at most 2 at a time, at most 1 hour each); `download_media` returns
  `status: "downloading"` and the same call later returns progress or the
  path. Stopping the server cancels it and removes the partial file; after a
  crash the next start removes it. One audit line is written per download,
  when it ends.
- Flood waits up to a minute are absorbed; longer ones fail the call. Each
  tool call times out after 55 s.

---

## Measurement plan (Saved Messages only)

All Telegram actions happen in `me`. Nothing is sent anywhere else.

Setup (needs the user's OK, because it edits their `config.toml`):
1. Temporarily set `max_upload_mb = 200` and `max_download_mb = 200`.
2. In the first `allowed_roots` dir, create `bench-150mb.bin` (150 MB) and
   `bench-30mb.bin` (30 MB) from `/dev/urandom` (random data, so nothing
   compresses and nothing private is uploaded).
3. `send_file` each to `me` with `as_document: true`; write down both
   message ids. These are the only messages the test creates.

Runs (each through the MCP tool of the running server, timed by the tool
call's duration; one `serve` primary at a time, restarting it between
builds):

| Build | 30 MB | 150 MB |
| --- | --- | --- |
| `main` (baseline) | time | expected timeout at 55 s |
| commit 1, threads 4 / 8 / 16 | time | time, or bytes at 55 s |
| commit 2 | — | time to `done` over repeated calls; progress values |

Thread variants are temporary local builds; only the chosen constant is
committed. Each run downloads into a fresh subdir so `name (1)` does not
mix runs.

Saved-Messages uploads are stored on the **home DC**, so these runs measure
the parallel gain, not the `FILE_MIGRATE` fix. The DC routing is covered by
unit tests; optionally, if the user names a message with a large file in a
chat on another DC, it can be **forwarded into `me`** (the forward keeps the
file on its DC) and timed the same way, then that forwarded copy deleted.

Cleanup:
1. `delete_messages` in `me` with exactly the recorded test message ids
   (and the optional forwarded one) — nothing else.
2. Remove the local `bench-*.bin` files and downloaded copies.
3. Restore `max_upload_mb` / `max_download_mb`.

Results (MB/s per build and file) go into the commit 1 message.

## Not doing

- No new tool (no `download_status` / `cancel_download`).
- No resume across restarts (would need a persisted part map).
- No CDN (`WithAllowCDN`) — separate change if measurement shows redirects.
- No home-DC connection pool unless measurement shows it is needed.

## Open questions for review

1. Thread count 8, pool 4 connections — OK to start there?
2. 2 concurrent jobs, 1 h cap, 2 min stall, 30 min result TTL — OK?
3. A third download refused rather than queued — OK?
4. `partial_downloads` table for crash cleanup — OK, or is a startup-only
   sweep of the root dirs (no subdirs) enough?
5. May the measurement temporarily raise `max_upload_mb`/`max_download_mb`
   to 200 in `config.toml`?
