# telegram-mcp

A Telegram [MCP](https://modelcontextprotocol.io) server shipped as one static
binary: no Python, no `uv`, no Docker. It is a focused Go rewrite inspired by
[chigwell/telegram-mcp](https://github.com/chigwell/telegram-mcp), built on
[gotd/td](https://github.com/gotd/td).

> **Status: early.** Login, secure session storage, 17 tools (messages, media,
> forum topics, HTML formatting) and one-command install work (see [Roadmap](#roadmap)).

## Install

Release binaries are built for **Apple Silicon Macs** (darwin/arm64) and
**Linux** (amd64, arm64).

```bash
curl -fsSL https://raw.githubusercontent.com/itskamol/telegram-mcp/main/install.sh | sh
```

It downloads the latest release, verifies its SHA-256
against `checksums.txt` and installs `telegram-mcp` into `~/.local/bin`
(`BIN_DIR=/usr/local/bin` to change, `VERSION=v0.2.0` to pin).

Homebrew:

```bash
brew install itskamol/tap/telegram-mcp
```

The binaries are not signed or notarized by Apple. `install.sh` downloads
with curl, which sets no quarantine flag, and the Homebrew cask clears it on
install, so Gatekeeper does not block either. A binary downloaded by hand
from Releases in a browser needs `xattr -d com.apple.quarantine telegram-mcp`.

On Linux the session key is kept in the Secret Service (GNOME Keyring,
KWallet) over D-Bus, so a desktop session or a running keyring daemon is
needed; on a headless server `login` fails until one is available.

### Update

```bash
telegram-mcp update           # to the latest release
telegram-mcp update --check   # only say whether there is one
telegram-mcp update --version v0.9.1   # a given release, also older
```

It downloads the release for this OS and CPU, checks it against
`checksums.txt`, and replaces the binary in place. Then restart the MCP
clients that use it (Claude Desktop, Claude Code sessions) and any
`telegram-mcp watch`: a server keeps running its old binary until then,
and a relay of a new version does not talk to a server of an older one.
Installed with Homebrew, update with `brew upgrade --cask telegram-mcp`
instead; `update` says so and leaves the file alone. Before `update`
existed (0.9.2 and older), rerun the `install.sh` line above.

Intel Macs and Windows: build from source (Go 1.26+):

```bash
go install github.com/itskamol/telegram-mcp-go/cmd/telegram-mcp@latest
```

## Set up

1. Create an app at <https://my.telegram.org/apps> to get `api_id` and `api_hash`.
2. Create `~/.config/telegram-mcp/config.toml`:

   ```toml
   api_id = 1234567
   api_hash = "0123456789abcdef0123456789abcdef"
   ```

   `TELEGRAM_API_ID` / `TELEGRAM_API_HASH` override the file.
3. Log in:

   ```bash
   telegram-mcp login          # phone number + code (+ 2FA password)
   telegram-mcp login --qr     # or scan a QR code from Telegram > Settings > Devices
   ```

`telegram-mcp status` shows who is logged in; `telegram-mcp logout` ends the
session on Telegram's side and deletes it locally. `--account work` keeps
several accounts side by side.

## Use with Claude

```bash
telegram-mcp install claude-desktop   # edits claude_desktop_config.json, keeps a .bak
telegram-mcp install claude-code      # runs `claude mcp add --scope user ...`
```

Options: `--name telegram`, `--read-only`, `--account work`, `--force` (replace
an existing entry with the same name). Only the binary path and arguments are
written; no secrets go into client configs.

Or by hand, in `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "telegram": {
      "command": "/usr/local/bin/telegram-mcp",
      "args": ["serve"]
    }
  }
}
```

Or for Claude Code:

```bash
claude mcp add telegram -- telegram-mcp serve
```

## Tools

| Read | Write |
|---|---|
| `get_me` | `send_message` |
| `list_chats` | `send_file` |
| `resolve_username` | `forward_messages` |
| `search_contacts` | `edit_message` (destructive) |
| `list_messages` | `delete_messages` (destructive) |
| `get_message` | `mark_chat_read` |
| `search_messages` | `download_media` (local disk only) |
| `list_topics` | `set_feed_filter` (local consumer settings) |
| `get_media_info` | |
| `view_photo` | |
| `get_new_messages` (with `watch_chat_ids` or `watch_all`) | |

`get_new_messages` and `set_feed_filter` appear only when `config.toml`
watches chats (see [Watching chats](#watching-chats)).

Conventions the tools share, so a model can carry what it learned from one
tool to the next:

- **Names** are `verb_noun`: `list_` returns a collection, `get_` one object,
  `search_` a query; plural nouns take several ids (`delete_messages`).
- **`chat`** (also `from_chat`, `to_chat`) takes the `id` from `list_chats` as
  a number or a string (Bot API style: users positive, groups negative,
  channels `-100…`), an `@username`, or `me`. `list_chats query=` finds a chat
  by title; `search_contacts` lists contacts first. Both always return
  `is_contact` (`true` only for users in the account's contacts), so a
  stranger with the same name is not mistaken for one.
- **Replies and topics**: `reply_to` and `topic_id` work the same in
  `send_message` and `send_file`; `list_topics` gives topic ids.
- **`parse_mode`**: `plain` (default), `markdown` (CommonMark: `**bold**`,
  `*italic*`, `` `code` ``, `[text](url)`, `> quote`, `||spoiler||`) or `html`.
  Markup is parsed locally, so broken markup fails before anything is sent,
  and the length limits apply to the text after markup is removed.
- **`idempotency_key`** on `send_message`, `send_file` and `forward_messages`:
  the key becomes Telegram's `random_id`, so a retry with the same key returns
  the message that was already sent (same `message_id`) instead of sending a
  second copy. A key counts per tool and chat. Timeouts of write tools say
  whether a retry is safe. `edit_message` needs no key: editing a message
  to the text it already has succeeds with `unchanged: true`.
- **Paging**: `list_messages` and `search_messages` return `has_more` and
  `next_offset_id` to pass back as `offset_id`. `get_message` reads one
  message by id; a deleted id, or one from another chat, is an error that
  says retrying will not help.
- **Media**: `media` names the attachment kind. `link_preview` is only the
  preview of a link in the text, not a file; stickers carry `sticker_emoji`.
  `send_message disable_link_preview=true` sends a link without a preview.
- **Dates** are in the machine's local time zone with their UTC offset;
  `get_me` returns the current local time.
- **Stale tool lists**: a client can keep an older server's tool
  definitions for the rest of a session, even after a reconnect (seen in
  Claude Code). `get_me`'s description names the version it came from and
  its answer the version that runs; when they differ, start a new session.
- **Errors** say what to do next; Telegram's code stays in brackets, e.g.
  `only messages sent by this account can be edited [MESSAGE_AUTHOR_REQUIRED]`.

## Watching chats

MCP tools are pulled, Telegram pushes. With chats listed in `config.toml`,
`serve` receives Telegram updates and buffers new and edited messages of
those chats, and the `get_new_messages` tool hands them out:

```toml
watch_chat_ids = [-1001889276137]
watch_topics = { "-1001889276137" = [1] }   # optional: only General
feed_retention_days = 7
```

`watch_all = true` buffers every chat of the account instead. It is off by
default because it writes every incoming message to disk. Telegram's
service chat `777000`, which sends login codes, is never buffered, and
`allowed_chat_ids` still limits it. `watch_topics` filters of listed chats
apply with it too. With `watch_all` a new consumer reads nothing until it
chooses its chats with `set_feed_filter` (see Filters below).

- **Consumers**: the server keeps each consumer's position (`consumer`,
  default `"default"`), so a new chat session continues where the last one
  stopped. A consumer used for the first time starts with everything still
  buffered, up to `feed_retention_days` old. `wait_seconds` (up to 45) waits
  for the next message instead of answering empty.
- **Acknowledging**: pass the previous answer's `cursor` as `ack_cursor` once
  those messages are handled. It acknowledges everything up to and including
  that cursor, and the same call returns the next batch, which stays
  unacknowledged until its own cursor comes back. Without `ack_cursor` the
  same batch comes again, so a session that dies mid-batch loses nothing
  (at-least-once). Acknowledging does not mark messages read on Telegram;
  `mark_chat_read` does that, and senders see it; `max_id` stops it at the
  last message you handled.
- **`seq` and `cursor`** are positions in the buffer, not message ids. One
  counter runs across all buffered chats, and an edit takes a new value, so
  a consumer sees gaps in `seq` all the time; only `gap: true` means messages
  were lost (expired by retention before they were acknowledged).
- **Filters**: `set_feed_filter` sets what a consumer reads, and
  `get_new_messages` only reads and acknowledges. `chat_ids` narrows it to
  some of the watched chats (`[]` goes back to all of them); `direction` is
  `in` (messages to the account), `out` (its own) or `both` (the default),
  e.g. `set_feed_filter(consumer="pos", chat_ids=[-1001889276137], direction="in")`.
  The direction follows Telegram's `out` flag, which Telegram never sets in
  Saved Messages (seen live, in history and in updates): there every
  message counts as `in`, the agent's own replies included. An agent that
  both reads and writes Saved Messages must skip the ids `send_message`
  returned to it.
  The filter is saved for that consumer, so later calls and
  `telegram-mcp watch --consumer pos` follow it, and every
  `get_new_messages` answer shows it under `filter`. A field left out keeps
  its value. Filters only narrow: an agent cannot add a chat that
  `config.toml` does not watch.
- **A filter change applies after the acknowledged cursor.** One cursor
  serves all chats and both directions, so messages the old filter skipped
  before it never come back; `list_messages` still has them.
- **`chat_ids` on `get_new_messages`** was removed in 0.8.0, after one
  release as a deprecated alias. A session still holding the old tool list
  gets an error naming the field; use `set_feed_filter(chat_ids=…)`.
- **Edits** replace the buffered message and deliver it again, with the same
  `id` and `edited: true`; **deletes** remove it. `out: true` marks the
  account's own messages, except in Saved Messages (see above). A message
  this server sends with `send_message` into another private chat can be
  missing: Telegram answers that call with the new id only, not the
  message. In Saved Messages they do arrive.
- **Topic filters fail closed**: a message whose forum topic cannot be told
  is not buffered.
- **No lost updates**: gotd's update state lives in the same SQLite database
  and Telegram redelivers what came while the server was off. The handler
  never reports an error to gotd (which would still advance pts): a write is
  retried, then recorded in `feed_dead_letters`, and if even that fails the
  server stops before that update's pts is saved.
- Watching is read-only. Messages in the buffer are written by others:
  agents should not act on them without the user.

### Waking an agent: `telegram-mcp watch`

An MCP server cannot wake an idle agent; tools only run when called. A
background command whose output the agent watches can (Claude Code's
Monitor tool notifies on each line). `telegram-mcp watch` is that command:

```bash
telegram-mcp watch --consumer default
```

```json
{"chat_id":-1004325193310,"first_id":1142,"last_id":1143,"count":2,"edits":1}
```

- Only messages the consumer's filter lets through are announced: its
  `chat_ids` (all watched chats if it has none) and `direction`, so a
  consumer set to `in` is not woken by the account's own messages.
- One line per chat with new or edited messages: ids only, never message
  text, so nothing private ends up in notifications or logs. `first_id` and
  `last_id` are Telegram message ids, not `get_new_messages` cursors. An
  edit is announced like a new message; `edits` says how many of `count`
  are edits, so a batch with `edits` equal to `count` holds nothing new. The agent then calls
  `get_new_messages(consumer, ack_cursor)` to read and acknowledge.
- It never writes and never acknowledges; delivery guarantees stay with the
  consumer's server-side offset. Messages left unacknowledged for 5
  minutes are announced again in the same format, so an
  agent that died mid-batch is woken again; unacknowledged messages from
  before `watch` started are announced at once.
- It reads the database read-only next to the running server, without the
  profile lock, and holds no read transaction between checks, so the
  server's WAL checkpoints are never blocked. It checks `PRAGMA
  data_version` every 500ms (about 3 µs of CPU per check) and queries only when another process committed.
- It waits for a database or feed tables that do not exist yet, reopens
  the file if it is replaced, and exits when its output pipe closes or on
  Ctrl-C, `SIGTERM` or `SIGALRM`.
- A monitor with a time limit (Claude Code's Monitor has one) ends `watch`;
  the agent has to start it again. Nothing is missed in between: a new
  `watch` announces every message the consumer has not acknowledged.

## Safety settings

All optional, in `config.toml`:

```toml
read_only = true                         # no tool that changes Telegram (also: serve --read-only)
allowed_chat_ids = [123456789, -1001234567890]  # restrict every tool to these chats
max_delete = 20                          # message IDs per delete_messages call
allowed_roots = ["~/Downloads/telegram-mcp"]  # where download_media may write (this is the default)
max_download_mb = 50                     # per file
max_upload_mb = 50                       # per send_file file
```

- `read_only` removes every tool that changes Telegram. Tools that write
  only locally stay: `download_media` (files in `allowed_roots`),
  `get_new_messages` acknowledgements and `set_feed_filter` (consumer
  positions and filters in the local database).
- Text from Telegram is returned as JSON with invisible and control characters
  stripped, and tool descriptions tell the model not to follow instructions in it.
- Outside channels Telegram deletes by message ID alone, with no chat attached.
  `delete_messages` first checks that every ID belongs to the chat you named
  and refuses otherwise.
- Every write is appended to `~/.local/state/telegram-mcp/audit.log` (tool, chat,
  message IDs, error) without message text.
- `download_media` writes only inside `allowed_roots` (symlinks resolved, `..`
  rejected), reduces file names to one safe component, never overwrites an
  existing file, and checks the size before, during and after the download.
  Data goes to a hidden `.telegram-mcp-*.part` file next to the target and
  becomes visible only when every part has arrived.
- A download that does not finish within about 45 s goes on inside the
  server (at most 2 at a time, at most 1 hour each, and ended if no data
  arrives for 2 minutes): `download_media` answers
  `status: "downloading"` with the bytes so far, and the same call later
  returns progress or the path. A message is never downloaded twice at once.
  Stopping the server cancels running downloads and removes their partial
  files; after a crash, the next start removes them (only files named
  `.telegram-mcp-*.part` that it recorded, inside `allowed_roots`). One
  audit line is written per download, when it ends.
- `send_file` is the risky direction (local files leave the machine), so it
  only reads regular files inside `allowed_roots`, refuses hidden paths such
  as `.env` or `.ssh/` (also through symlinks), and checks that the file it
  opened is the one it validated.
- `view_photo` shows the model a copy scaled to at most 1280 px (5 MB cap).
- On macOS, `~/Downloads` (the default root), `~/Documents` and `~/Desktop`
  need Files and Folders permission for the app that runs `telegram-mcp`.
  Without it the file tools fail with "operation not permitted" and say so;
  grant the permission or move `allowed_roots` elsewhere.
- Flood waits up to a minute are absorbed; each tool call times out after 55 s.
- Only the user decides what is buffered (`watch_chat_ids`, `watch_all`);
  `set_feed_filter` can narrow that, never widen it. Login codes
  from Telegram's `777000` are never buffered, even with `watch_all`.
- With `watch_all`, a new consumer must choose its chats before its first
  read: `get_new_messages` refuses, before any message reaches the agent,
  until `set_feed_filter` sets `chat_ids` (`[]` chooses every chat on
  purpose). An agent started for one group does not get every private chat
  by default. Consumers that existed before this rule keep reading as they
  did. `watch` cannot tell and still announces such a consumer's chats, ids
  only; the agent then gets the refusal.

## Where files live

Follows the XDG base directory layout, on macOS too (like `gh` and `git`):

| File | Location |
|---|---|
| `config.toml` (you edit it) | `$XDG_CONFIG_HOME/telegram-mcp`, default `~/.config/telegram-mcp` |
| `telegram-mcp.db` (session, peer cache) | `$XDG_DATA_HOME/telegram-mcp`, default `~/.local/share/telegram-mcp` |
| `audit.log` | `$XDG_STATE_HOME/telegram-mcp`, default `~/.local/state/telegram-mcp` |

The database stays out of `~/.config`, so a dotfiles repository never picks
up the session. `TELEGRAM_MCP_HOME=/some/dir` puts all three files in one
directory instead. On Windows everything is in `%AppData%\telegram-mcp`.

Profiles from earlier versions (`~/Library/Application Support/telegram-mcp`
on macOS) are moved automatically on first run; nothing is moved if any
destination file already exists or a server is still running from the old place.

## How the session is stored

| What | Where |
|---|---|
| Session (auth key) | SQLite, encrypted with AES-256-GCM |
| Encryption key | OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service) |
| Peer cache | SQLite |
| `api_id`, `api_hash` | `config.toml` |

- The session never sits in plain text: not in `.env`, not in the MCP client
  config, not in the database.
- Directories are `0700` and files `0600`.
- A file lock lets only one process use a profile. Two connections on one auth
  key make Telegram return `AUTH_KEY_DUPLICATED` and drop both.
- A session row is bound to its account name, so it cannot be copied under
  another account.

## Roadmap

- [x] Login (phone, QR, 2FA), status, logout
- [x] Encrypted SQLite session, keychain key, single-process lock
- [x] MCP server over stdio with flood-wait handling and per-tool timeouts
- [x] 17 core tools, plus 2 for watched chats (see [Tools](#tools))
- [x] Safety: output sanitizing, chat allowlist, read-only mode, audit log
- [x] Media: `get_media_info`, `download_media` (allowed roots, size limit), `view_photo`
- [x] HTML formatting for sent messages, forum topics
- [x] One-line installer, `install claude-desktop|claude-code`
- [x] Homebrew tap (`brew install itskamol/tap/telegram-mcp`)

## License

Apache-2.0
