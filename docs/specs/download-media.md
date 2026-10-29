# Spec: fast `download_media` that can outlive one tool call

Status: approved. Nothing here is implemented yet.

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
// Pools opens connections to other DCs and reports the home DC;
// *telegram.Client satisfies it.
type Pools interface {
	DC(ctx context.Context, dc int, max int64) (telegram.CloseInvoker, error)
	Config() tg.Config
}
```

The home DC is read per download from `Config().ThisDC`. Where gotd
writes it (v0.163.0): `client.go` starts with an empty `tg.Config`
(`ThisDC == 0`); `onSession` (`session.go`) stores the config each time the
primary connection is established, including after `migrateToDc`, which
restarts the primary on the new DC; `fetchConfig` (`cfg.go`) stores it again
on `updateConfig`. So the value does follow a migration. In practice the
home DC of a logged-in account does not change (`USER_MIGRATE` comes during
login, `PHONE_MIGRATE`/`NETWORK_MIGRATE` before or around it); reading it
per download is a cheap guard, not something the design relies on.

**`ThisDC == 0`** (config not loaded yet) is treated like an unknown file
DC: the home connection is used and no pool is opened. Otherwise every file
would look "not home", and a file on the home DC would get a pool with an
auth transfer to the account's own DC. In `serve` this cannot happen, since
`Service` is built after `c.Self` succeeded, but the rule does not depend
on that.

`Service` keeps a cache of opened DC pools (`map[int]*dcEntry` behind a
mutex), closed by `Service.Close`. `c.DC` exports and imports the
authorization on first use (`auth.exportAuthorization` /
`auth.importAuthorization`), so caching the pool avoids that handshake on
every download.

Routing:

| File DC | Invoker | Why |
| --- | --- | --- |
| home DC | `s.API` (the existing connection) | MTProto pipelines many in-flight requests on one connection, so the concurrent requests still overlap. No extra connection on the primary auth key, which keeps us clear of anything that could look like `AUTH_KEY_DUPLICATED`. **This is a hypothesis:** one TCP connection still has one congestion window and possibly a server-side per-connection limit, so 8 concurrent requests may not give 8× here. The benchmark measures exactly this path, and the commit message says it is a hypothesis. |
| other DC | cached `Pools.DC(ctx, dc, downloadConcurrency)` | Requests go straight to the DC that has the file: no `FILE_MIGRATE`, one round trip per part. The pool uses the DC's exported key, not the primary one. |
| `DC == 0` (unknown), or home DC unknown (`ThisDC == 0`) | `s.API` | Same as today; gotd's migrate handling still works. |

**Pool size = number of concurrent requests.** A gotd `pool.DC` connection carries one request
at a time: `acquire` takes it off the free list and `release` returns it only
after the response (`pool/pool.go`, `DC.Invoke`). With 4 connections and 8
requests in flight, 4 would just wait. So the pool's max is `downloadConcurrency`.

**One pool per DC, shared by jobs — on purpose.** Two jobs on the same DC
(up to 2 run at once, see 2.2) share its 8 connections: 16 requests queue
for 8 connections, so each job runs at roughly 4 in flight. That is the
intended trade-off, not a bug: the number of connections this account opens
to one DC stays fixed at 8 no matter how many downloads run, which keeps the
footprint predictable and away from flood limits. Do not "fix" it with a
pool per job.

If measurement (below) shows the home DC is the bottleneck, `c.Pool(n)` for the
home DC is a later, separate change, tried live in Saved Messages first,
because those connections share the primary auth key.

#### Pool lifecycle (checked in gotd v0.163.0 source)

- **Who owns it.** The pool's lifetime belongs to the `telegram.Client`:
  `createPool` builds `pool.NewDC(c.ctx, …)` on the client's own run context,
  not on the `ctx` passed to `c.DC`. That `ctx` bounds only the first
  auth transfer (`c.transfer`). `Service` owns the *cache* and closes every
  pool in `Service.Close`, which `runServe` calls inside the `c.Run`
  callback, before the client stops.
- **Which ctx opens it.** Not the tool call's. The pool is opened on a
  context derived from the server context (`Service.ctx`, set in
  `NewService`) with a 30 s timeout. The caller waits for the result with
  its own ctx (`select` on a done channel and `ctx.Done()`), so a cancelled
  first call stops only its own wait and cannot fail or poison the opening
  for anyone else.
- **One opening per DC.** The cache entry is created under the mutex with a
  `done` channel before the mutex is released; concurrent first requests
  for the same DC wait on that channel. (Plain mutex + channel; no new
  dependency for `singleflight`.) The opener goroutine closes `done` in a
  `defer`, so a panic or an early return can never leave waiters hanging;
  a recovered panic becomes the entry's error.
- **`Close` vs. an opening in progress.** An opener may still be inside
  `c.DC` (up to 30 s) when `Service.Close` runs. If `Close` just closed the
  pools it found, the opener would then store a fresh pool in a cache nobody
  closes again: a leaked pool. So, both sides under the same mutex:
  - `Close` sets `closed = true`, cancels the openers' context (the 30 s one
    derives from it, so `c.DC` returns quickly), then waits for every opener
    (`sync.WaitGroup` that each opener joins under the mutex when it is
    started), and only then closes every pool in the cache.
  - An opener that finishes stores its pool only if `closed` is false;
    otherwise it closes the pool itself and reports "service closed" to its
    waiters.
  - `pool()` called after `closed` returns "service closed" without
    starting an opener.

  Either rule alone would close the leak; with both, `Close` also returns
  only when no opener goroutine is left, which the shutdown order (2.3)
  relies on.
- **Errors are not cached.** If `c.DC` fails (e.g. `exportAuthorization`
  error, timeout), gotd closes the half-made pool itself; the entry is
  removed from the map, every waiter gets the error, and the next download
  tries again.
- **Dead connections.** gotd replaces them: `acquire` checks `Dead()` on a
  free connection and creates a new one, and `DC.Invoke` retries a request
  whose connection died before the server processed it
  (`errRetryableOnNewConn`). The pool as a whole only stops when the client's
  context ends or we call `Close`, i.e. at shutdown. So no replacement
  logic; as a guard, an invoke error from a closed pool ("DC is closed")
  drops the cache entry so a later download reopens it.

This all runs in the **primary** process only. Relays (`internal/hub`) never
open Telegram connections, so the one-process-per-profile rule holds.

### 1.2 Part size and concurrency

```go
downloader.NewDownloader().WithPartSize(1 << 20).
	Download(tg.NewClient(inv), loc).WithThreads(downloadConcurrency).Parallel(ctx, w)
```

`Download` takes a `downloader.Client` (an interface of `UploadGetFile`,
`UploadGetFileHashes`, … methods), which `*tg.Client` satisfies and a bare
`tg.Invoker` does not; hence `tg.NewClient(inv)` around the pool.

- **Part size 1 MB.** That is the maximum `upload.getFile` allows (limit
  must divide 1 MB and a part must not cross a 1 MB boundary; 1 MB parts at
  1 MB offsets always qualify). It halves the request count against 512 KB.
- **Concurrency: 8 requests in flight** (`WithThreads` starts that many goroutines, not OS threads; unrelated to `GOMAXPROCS`). Reasoning:
  - 8 × 1 MB in flight is enough to fill a typical link at 100–150 ms RTT
    (≈ 50–80 MB/s ceiling), so the link, not latency, becomes the limit.
  - Memory stays bounded: 8 parts being fetched + gotd's write channel of 8
    = at most ~16 MB per download.
  - Official clients keep a similar number of requests per file in flight;
    16+ buys little and raises the chance of `FLOOD_WAIT`.
  - On another DC that means 8 pool connections (one request each, see
    1.1); on the home DC, 8 requests pipelined on the one connection.
  - The measurement plan sweeps 4 / 8 / 16 and the constant is changed if 4
    or 16 is clearly better. It stays a constant, not a config option.

### 1.3 Writing parts out of order: `fsguard.WriteNewAt`

`Parallel` needs an `io.WriterAt`; `fsguard.WriteNew` takes an `io.Writer`.
Add a sibling and keep `WriteNew` for other callers:

```go
// ErrBadPart means a WriteAt did not fit the part layout: misaligned,
// short before the end, past the end, or a part written twice.
var ErrBadPart = errors.New("download part does not fit the file")

// ErrIncomplete means write returned without filling every part.
var ErrIncomplete = errors.New("download is incomplete")

// PartWriter is the io.WriterAt that WriteNewAt hands to write.
type PartWriter struct{ /* temp file, size, partSize, bitmap, written, err */ }

func (w *PartWriter) WriteAt(p []byte, off int64) (int, error)
func (w *PartWriter) Written() int64 // bytes accepted so far (progress)
func (w *PartWriter) Reset() error   // truncate, clear bitmap and written (1.5)

// WriteNewAt is WriteNew for writers that fill the file out of order, in
// parts of partSize bytes. size is the exact expected length and must be
// > 0; a size above limit fails before anything is created. The file is
// kept only if every part was written exactly once.
func WriteNewAt(dir, name string, size, limit, partSize int64, write func(*PartWriter) error) (string, error)
```

A refused `WriteAt` is **sticky**: the first `ErrBadPart` is remembered and
`WriteNewAt` returns it even if `write` swallowed it and returned nil. A
refused write means the downloader and the file layout disagree, and that
must never end in a kept file.

`tele` does not import `fsguard`; `Service.DownloadFile` takes the small
interface it needs (`interface{ io.WriterAt; Reset() error }`), which
`*fsguard.PartWriter` satisfies.

How gotd ends a parallel download (`downloader/parallel.go`, `reader.go`,
`sink.go`, v0.163.0): each request goroutine takes the next offset
(`nextPlain` adds `partSize` under a mutex) and fetches it. A chunk with
`len < 1` stops the download **before** it is sent to the write channel, so
the write loop never calls `WriteAt` with an empty slice. A chunk shorter
than `partSize` is written and then stops the download. For a file of
exactly N MB, the last full part is written normally and the following
offset comes back empty and is dropped. Requests already in flight past the
end also come back empty and are dropped: a few wasted requests, no writes.
A short chunk in the middle of a file (which Telegram should never send)
would fail the "short only at the end" rule above instead of passing
silently.

`size` must be > 0: `WriteNewAt` refuses 0 (and negatives) before creating
anything and never calls `write`. 0 is not "unknown" there, and not "an
empty file" either, because a caller that confused the two would silently
get an empty file. Media with an unknown size (`f.Info.Size == 0`, rare) keeps the sequential
`Stream` into `WriteNew`, so `WriteNewAt` never has to guess the length.

Every guarantee of `WriteNew` is kept:

| Guarantee | How |
| --- | --- |
| Inside `allowed_roots`, symlinks resolved | Unchanged: `dir` still comes from `ResolveDir`, which is called before any download. |
| Never overwrite | Unchanged: data goes to `os.CreateTemp(dir, ".telegram-mcp-*.part")`, then `os.Link` to the first free `name`, `name (1)`, …; `Link` fails on an existing file. The temp file is removed in every case. Shared code with `WriteNew` moves into one helper. |
| Size checked **before** | `size > limit` → `ErrTooLarge` before the temp file exists (the handler also checks `f.Info.Size` as today). |
| Size checked **during** | `PartWriter.WriteAt` refuses any write that ends past `size` (`ErrBadPart`), and `size <= limit` was checked before, so the file can never grow past `limit`. Out-of-order writes make a running total meaningless, so the bound is on the end offset. No separate `limitWriterAt` is needed. |
| **Complete**, not just the right length | A file of the right length can still have a hole: if a middle part is never written and the last one is, the `.part` is a sparse file with a run of zero bytes and the correct size. So the length alone proves nothing. The writer tracks coverage (one bit per part; 200 MB = 200 bits). A `WriteAt(p, off)` is accepted only if `off % partSize == 0`, its part's bit is not yet set, `off+len(p) <= size`, and **either** `len(p) == partSize` **or** `len(p) < partSize` and `off+len(p) == size` (the last part; when `size` is an exact multiple of `partSize`, the last part is full-length and passes the first rule). `len(p)` is never more than `partSize`: one write that ends at `size` but spans two parts would otherwise mark only its first part. The bit is checked and set in **one** critical section, so two writers of the same part cannot both pass. Anything else (misaligned, short in the middle, past `size`, or a repeat) is an error. `len(p) == 0` is accepted as a no-op that sets no bit: gotd does not send it today (see below), and if a future version does, the final coverage check still catches any missing data. After `write` returns, every part's bit must be set **and** the atomic `written` counter must equal `size`, else an error and nothing is kept. Since writes cannot overlap, these two checks together mean every byte came from Telegram. |
| `.part` then rename | Unchanged (link + remove, as above). |
| Concurrency | `*os.File.WriteAt` is safe for concurrent use (`pwrite`); gotd also serialises through `syncio.NewWriterAt`. The part bitmap is under a small mutex; `written` is atomic (commit 2 reads it for progress). |
| Restart (1.5) | `Reset()` clears the bitmap and sets `written` to 0. **Contract, enforced:** `WriteAt` counts itself as in flight in the same critical section that marks its part, and uncounts after the disk write returns. `Reset` takes the mutex and, if any write is in flight, returns `ErrWriteInFlight` and changes nothing; otherwise it clears. So a write that marked its part before `Reset` can never land in the next attempt and inflate `written`. A `WriteAt` that has not yet reached its critical section when `Reset` runs is, by construction, a write of the new attempt; the refetch check (1.5) guarantees the same file, so its bytes are right, and if it duplicates a part the new attempt also writes, the sticky `ErrBadPart` fails the download: a failure, never a wrong file. **Caller:** `ErrWriteInFlight` can only appear if the assumption "when `Parallel` returns, every write has returned" is broken, i.e. a bug. `DownloadFile` treats it as fatal: the download fails at once with that error, with no retry and no waiting for the write to finish. `Reset` does **not** truncate: the coverage check already requires every part to be written again, so old bytes cannot survive (a mutation test confirmed truncation is not load-bearing), and not truncating keeps the file's blocks allocated for the second attempt. `Reset` keeps a sticky refusal: a refusal means the downloader and the part layout disagreed, which is a bug, not an expired reference, and a retry must not hide it. |

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
- The home-DC path goes through the client's waiter, which sleeps waits
  ≤ 60 s but **returns** longer ones as the original `FLOOD_WAIT` error, which
  gotd's downloader would then sleep through. So the same `floodLimit`
  wrapper sits on top there too (`floodLimit(s.API's invoker)`), between the
  downloader and the waiter. Both paths then behave the same: ≤ 60 s slept,
  > 60 s fails.

Every sleep is still bounded by the context (tool call or, in commit 2, job).

### 1.5 `FILE_REFERENCE_EXPIRED`

File references expire (hours, sometimes sooner). On
`FILE_REFERENCE_EXPIRED` the download is retried **once** from the start:
the message is fetched again with `Service.Media` (fresh `FileReference`),
the same temp file is reset (truncated, bitmap and `written` cleared, see
1.3) and refilled. The message may have been edited meanwhile, so the
refetched file must be **the same file**: same kind, same `Document.ID` /
`Photo.ID`, same size. Otherwise the download fails with "the message's file
changed during the download; call again" and nothing is kept: the new file
would otherwise be written under the old name and the old size limit. gotd's
`Parallel` cannot
resume at an offset, and a mid-download expiry is rare, so a restart is
simpler than a resumable reader. A second expiry fails with the existing
hint ("the file reference expired; fetch the message again and retry").

This means `Service.Download` takes the chat + message (or a refetch
callback) instead of a bare location; the new signature:

```go
// PartSink is where DownloadFile writes; Reset empties it for a restart.
type PartSink interface {
	io.WriterAt
	Reset() error
}

// DownloadFile writes f to w in parallel on f's DC. refetch reloads the
// message when the file reference expired.
func (s *Service) DownloadFile(ctx context.Context, f MediaFile, w PartSink,
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
- Last part never written (file shorter than `size`) → `ErrIncomplete`,
  nothing left behind.
- A refused `WriteAt` that `write` swallows (returns nil) → `WriteNewAt`
  still returns `ErrBadPart`, nothing left behind.
- **Hole:** all parts except one middle part are written (so the length is
  right and the file is sparse) → error, nothing left behind.
- Misaligned offset, wrong part length, and the same part written twice →
  each an error.
- Size edge cases, with `partSize` = 1 MB, each written in shuffled order
  and checked byte for byte: **1 byte** (one short last part), **exactly
  2 MB** (two full parts, no short part), **2 MB + 1 byte** (two full parts
  plus a 1-byte last part).
- A short part that is not the last (e.g. 512 KB at offset 0 of a 2 MB file)
  → error; a part ending past `size` → error.
- `WriteAt` with an empty slice → no error, no bit set; a file whose only
  "write" was empty fails the final check.
- `Reset()` after some parts, then a full write → correct bytes,
  `Written() == 0` right after `Reset`.
- `Reset()` while a write is inside its disk write → `ErrWriteInFlight`,
  file untouched; after the write returns, `Reset` works.
- `size` 0 or negative → error, `write` never called, nothing created.
- Existing `name` → result is `name (1)`; the existing file is untouched.
- Existing tests for `WriteNew` keep passing (shared helper).

`internal/tele` (fake `tg.Invoker` as in `tele_test.go`, fake `Pools`):
- A document with `DCID` ≠ home goes to `Pools.DC(…, dc, 8)`, never to
  `s.API`; with `DCID` = home it goes to `s.API`; the pool is opened once for
  two downloads and closed by `Service.Close`.
- Two concurrent first downloads on one DC → `Pools.DC` called once.
- `Pools.DC` fails → both waiters get the error; the next download calls it
  again (error not cached).
- The first caller's ctx is cancelled while the pool opens → that call
  returns `context.Canceled`, a second waiting caller still gets the pool.
- `FILE_REFERENCE_EXPIRED`, refetch returns a different `Document.ID` (or
  size) → "file changed" error, no file left.
- **`Close` during an opening:** a fake `Pools.DC` that blocks until its
  context ends; `Close` returns only after the opener has exited, the pool
  the fake hands back is closed (the fake records `Close`), and the waiting
  caller gets "service closed". A `pool()` call after `Close` → "service
  closed", `Pools.DC` not called.
- A fake `Pools.DC` that panics → waiters get an error, none hangs (test
  with a timeout).
- Requests use `limit = 1 MB` and offsets that are multiples of 1 MB:
  checked in every download test by the fake DC itself, which answers
  `LIMIT_INVALID` / `OFFSET_INVALID` like Telegram and serves no bytes past
  the end.
- Pool opened with `max == downloadConcurrency`; `ThisDC == 0` → home
  connection, no pool.
- `FLOOD_WAIT_3` is retried; `FLOOD_WAIT_120` fails fast with a rate-limit
  message (no sleep: test runs with a short context).
- `FILE_REFERENCE_EXPIRED` → refetch called once, download succeeds; twice →
  error with the existing hint.
- `FILE_REFERENCE_EXPIRED` and the sink's `Reset` returns `ErrWriteInFlight`
  → `DownloadFile` fails with `ErrWriteInFlight`, does not refetch again,
  and does not call `Reset` a second time.

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

One `downloads` registry per primary process. It is created once in
`runServe` and passed to **both** `mcpserver.New` calls: the full server and
the read-only one share one `svc`, and `download_media` exists in both, so a
registry per `mcpserver.Server` would let a read-only session and a full
session start two copies of the same file.

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
- Before a `done` entry is returned, its `path` is checked with `os.Stat`:
  if the user deleted or moved the file, or its size is no longer `size`, the
  entry is forgotten and this call starts a fresh download. A path that no
  longer exists is never reported as done.
- **Concurrency:** at most **2 running jobs** (each up to 8 concurrent requests). A third
  distinct download is refused: "2 downloads are already running; try again
  when one finishes" — honest and simple, no hidden queue.
- **Limits per job:** context with a 1 h cap, and a stall watchdog: no new
  bytes for 2 min → the job fails ("download stalled").

### 2.3 Cancel and restart

- **Client cancels the tool call** (MCP `notifications/cancelled`, or the 55 s
  timeout): only the *wait* ends. The job runs on the server's context, not
  the call's, and continues; the next call sees it.
- **Server stops cleanly** (stdin closed, SIGINT/SIGTERM): every job is
  cancelled, its `.part` is removed by `WriteNewAt`'s cleanup, and an audit
  line with `error: "canceled: server stopped"` is written for each. The
  order matters, because each step needs what the next one closes:

  1. Stop accepting new jobs; cancel the jobs' context.
  2. Wait for every job goroutine (`sync.WaitGroup`). Each one, on its
     way out, removes its `.part`, deletes its `partial_downloads` row
     (needs the DB) and writes its audit line (needs the audit file).
  3. `Service.Close`: close the DC pools.
  4. The `c.Run` callback returns; the Telegram client stops.
  5. The deferred `auditLog.Close()`, then `e.close()` (the DB) run, in
     that order (Go defers are LIFO; both are already deferred that way in
     `runServe`).

  Steps 1–3 run inside the `c.Run` callback, in the existing deferred
  function, after `cancel()` and `wg.Wait()` have stopped the listeners and
  relayed sessions: `downloads.Shutdown()` then `svc.Close()`. The jobs'
  context is derived from the client's context, not from the listeners'
  `ctx`, so `cancel()` alone does not stop them; `Shutdown` does. Done the other way
  round, the `canceled` line goes to a closed file and is lost silently, and
  the row stays in a closed DB, so the next start sweeps a `.part` that is
  already gone.

  **A cancelled job never links its file.** The job's `write` callback ends
  with `return ctx.Err()` after the download, so a job cancelled at step 1
  returns an error and `WriteNewAt` removes the `.part` instead of linking
  it. The check sits in the job, not in `WriteNewAt`, which stays free of
  job logic. Between that check and the link there are only `Close`, the
  bitmap check and one `os.Link`; a job that passed the check before
  step 1 links and writes its audit line in that time, long before the
  10 s limit below. So a finished file without an audit line cannot appear.

  This is a **timing argument**, not a mechanism: the check and the link are
  a check-then-act pair, and the argument holds only while `Close` + `os.Link`
  take less than the 10 s abandonment limit. It breaks on a filesystem where
  `link` can hang (a stale NFS or SMB mount, a disk in error recovery): a job
  stuck in `os.Link` past 10 s is abandoned, the audit file is closed, and
  the link may then complete with no audit line. Accepted, because
  `allowed_roots` on such a mount is unusual and the stderr log names the
  abandoned job; a strict fix would hold the audit log open until every job
  goroutine has exited, at the cost of an unbounded shutdown.

  Step 2 has a 10 s limit: a job stuck past it is logged to
  stderr and abandoned. This is the one accepted exception to the order:
  its late audit and DB writes may hit closed handles and fail (logged to
  stderr, not lost silently), and its row stays, so the next start's sweep
  removes its `.part`.
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
  same message share one job, including when one is a `--read-only` relay
  and the other is not (one registry for both servers, see 2.2).
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
- **Shutdown order:** with a running job, `Shutdown` then `Close`; the test
  wraps the audit log and the `partial_downloads` store in fakes that fail
  after close, and checks that the `canceled` line was written and the row
  deleted *before* either was closed, and that `Shutdown` returned only after
  the job goroutine exited.
- A `done` entry whose file was deleted → the next call starts a new
  download instead of returning the missing path.
- A third concurrent job is refused; finished entries expire after the TTL
  (injected clock).
- Stall watchdog fires with a short injected timeout.

`internal/storage`: `partial_downloads` add / remove / list.

Startup sweep: removes a listed `.telegram-mcp-x.part` inside a root; keeps a
listed path outside the roots and a listed path with another name.

`internal/hub`: two relayed clients request the same message → one download
(extend the existing `hub_test.go` setup); and one full plus one read-only
server built with the same registry → one download.

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

Setup (approved by the user; it edits their `config.toml`):
0. **No other session on this profile** during the whole benchmark: close
   Claude Desktop chats/Cowork and other Code sessions that use
   telegram-mcp, and check that `serve` runs as a single primary with no
   relays. While the limits are 200 MB, any session on the profile could
   send or fetch 200 MB files, not only the benchmark.
1. Save a copy of `config.toml` (in the scratch dir, not the repo), then
   temporarily set `max_upload_mb = 200` and `max_download_mb = 200`.
2. In the first `allowed_roots` dir, create `bench-150mb.bin` (150 MB) and
   `bench-30mb.bin` (30 MB) from `/dev/urandom` (random data, so nothing
   compresses and nothing private is uploaded).
3. `send_file` each to `me` with `as_document: true`; write down both
   message ids. These are the only messages the test creates.

Timing comes from the **server's own log**, not the MCP call duration (that
includes client overhead). Commit 1 adds one stderr line per download at
the end: `download done dc=… concurrency=… bytes=… elapsed=…` (no path, no
chat content, nothing secret). `main` has no such line, so for the baseline
the same line is added in a local, uncommitted build.

Runs, one `serve` primary at a time, restarting it between builds:

| Build | 30 MB | 150 MB |
| --- | --- | --- |
| `main` (baseline) | time | expected timeout at 55 s (bytes written so far from the log) |
| commit 1, concurrency 4 / 8 / 16 | time | time, or bytes at 55 s |
| commit 2 | — | time to `done` over repeated calls; progress values |

Method, so the order of runs does not favour the new build:
- The second download of a file may be served "warm" from the DC's cache,
  so builds are **alternated** (A B A B …, A = `main`, B = commit 1), not
  run as all A then all B.
- At least **3 runs per build and file**; the **median** is reported, with
  min and max.
- Each run downloads into a fresh subdir so `name (1)` does not mix runs.

Concurrency variants are temporary local builds; only the chosen constant is
committed.

Saved-Messages uploads are stored on the **home DC**, so these runs measure
the parallel gain, not the `FILE_MIGRATE` fix. The DC routing is covered by
unit tests; optionally, if the user names a message with a large file in a
chat on another DC, it can be **forwarded into `me`** (the forward keeps the
file on its DC) and timed the same way, then that forwarded copy deleted.

Cleanup **always runs**, also when a run fails or is aborted half-way
(each step is attempted even if an earlier one failed, and failures are
reported, not skipped):
1. `delete_messages` in `me` with exactly the recorded test message ids
   (and the optional forwarded one) — nothing else.
2. Remove the local `bench-*.bin` files and downloaded copies.
3. Restore `max_upload_mb` / `max_download_mb`, then **verify**: diff
   `config.toml` against the copy saved before step 1 of setup (they must be
   identical), and `list_messages` in `me` shows none of the test ids.

Results (median MB/s per build and file, with min/max) go into the commit 1
message, together with the home-DC hypothesis and whether it held.

## Not doing

- No new tool (no `download_status` / `cancel_download`).
- No resume across restarts (would need a persisted part map).
- No CDN (`WithAllowCDN`) — separate change if measurement shows redirects.
- No home-DC connection pool unless measurement shows it is needed.

## Review decisions

1. 8 concurrent requests to start; the pool size follows it (8, see 1.1).
   Measurement decides the final number.
2. 2 concurrent jobs, 1 h cap, 2 min stall, 30 min result TTL: kept.
3. A third download is refused, not queued: a hidden queue would make
   `status: "downloading"` untrue.
4. `partial_downloads` table kept: `dir` allows subdirs, so a sweep of the
   root dirs alone would miss `.part` files, and a recursive sweep of a large
   root (e.g. `~`) is expensive.

5. The measurement may temporarily raise `max_upload_mb`/`max_download_mb`
   to 200 in `config.toml` (approved by the user); cleanup verifies the
   original is restored.

Status: reviewed and approved. Implementation is a separate step.
