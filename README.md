# telegram-mcp (Go)

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
curl -fsSL https://raw.githubusercontent.com/itskamol/telegram-mcp-go/main/install.sh | sh
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
| `search_messages` | `mark_chat_read` |
| `list_topics` | `download_media` (local disk only) |
| `get_media_info` | |
| `view_photo` | |
| `get_new_messages` (with `watch_chat_ids` or `watch_all`) | |

Conventions the tools share, so a model can carry what it learned from one
tool to the next:

- **Names** are `verb_noun`: `list_` returns a collection, `get_` one object,
  `search_` a query; plural nouns take several ids (`delete_messages`).
- **`chat`** (also `from_chat`, `to_chat`) takes the `id` from `list_chats` as
  a number or a string (Bot API style: users positive, groups negative,
  channels `-100…`), an `@username`, or `me`. `list_chats query=` finds a chat
  by title; `search_contacts` lists contacts first and marks them
  `is_contact`, so a stranger with the same name is not mistaken for one.
- **Replies and topics**: `reply_to` and `topic_id` work the same in
  `send_message` and `send_file`; `list_topics` gives topic ids.
- **`parse_mode`**: `plain` (default), `markdown` (CommonMark: `**bold**`,
  `*italic*`, `` `code` ``, `[text](url)`, `> quote`, `||spoiler||`) or `html`.
  Markup is parsed locally, so broken markup fails before anything is sent,
  and the length limits apply to the text after markup is removed.
- **`idempotency_key`** on `send_message`, `send_file` and `forward_messages`:
  the key becomes Telegram's `random_id`, so a retry with the same key returns
  the message that was already sent (same `message_id`) instead of sending a
  second copy. Timeouts of write tools say whether a retry is safe.
- **Paging**: `list_messages` and `search_messages` return `has_more` and
  `next_offset_id` to pass back as `offset_id`.
- **Dates** are in the machine's local time zone with their UTC offset;
  `get_me` returns the current local time.
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
apply with it too.

- **Consumers**: the server keeps each consumer's position (`consumer`,
  default `"default"`), so a new chat session continues where the last one
  stopped. Pass the previous answer's `cursor` as `ack_cursor` once those
  messages are handled; without it the same batch comes again, so a session
  that dies mid-batch loses nothing (at-least-once). `wait_seconds` (up to 45)
  waits for the next message instead of answering empty.
- **Choosing chats**: `chat_ids` narrows a consumer to some of the watched
  chats, e.g. `get_new_messages(consumer="pos", chat_ids=[-1001889276137])`.
  The server remembers it for that consumer, so later calls and
  `telegram-mcp watch --consumer pos` need not repeat it; `chat_ids=[]`
  goes back to all watched chats. It can only narrow: an agent cannot add a
  chat that `config.toml` does not watch.
- **Edits** replace the buffered message and deliver it again; **deletes**
  remove it. `gap: true` means unhandled messages expired by retention.
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
{"chat_id":-1004325193310,"first_id":1142,"last_id":1143,"count":2}
```

- Only chats in the consumer's `chat_ids` are announced (all watched chats
  if it has none).
- One line per chat with new or edited messages: ids only, never message
  text, so nothing private ends up in notifications or logs. The agent then
  calls `get_new_messages(consumer, ack_cursor)` to read and acknowledge.
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
  the file if it is replaced, and exits when its output pipe closes.

## Safety settings

All optional, in `config.toml`:

```toml
read_only = true                         # register only the read tools (also: serve --read-only)
allowed_chat_ids = [123456789, -1001234567890]  # restrict every tool to these chats
max_delete = 20                          # message IDs per delete_messages call
allowed_roots = ["~/Downloads/telegram-mcp"]  # where download_media may write (this is the default)
max_download_mb = 50                     # per file
max_upload_mb = 50                       # per send_file file
```

- Text from Telegram is returned as JSON with invisible and control characters
  stripped, and tool descriptions tell the model not to follow instructions in it.
- Outside channels Telegram deletes by message ID alone, with no chat attached.
  `delete_messages` first checks that every ID belongs to the chat you named
  and refuses otherwise.
- Every write is appended to `~/.local/state/telegram-mcp/audit.log` (tool, chat,
  message IDs, error) without message text.
- `download_media` writes only inside `allowed_roots` (symlinks resolved, `..`
  rejected), reduces file names to one safe component, never overwrites an
  existing file, and checks the size before and during the download.
- `send_file` is the risky direction (local files leave the machine), so it
  only reads regular files inside `allowed_roots`, refuses hidden paths such
  as `.env` or `.ssh/` (also through symlinks), and checks that the file it
  opened is the one it validated.
- `view_photo` shows the model a copy scaled to at most 1280 px (5 MB cap).
- Flood waits up to a minute are absorbed; each tool call times out after 55 s.
- Only the user decides what is buffered (`watch_chat_ids`, `watch_all`);
  `get_new_messages chat_ids` can narrow that, never widen it. Login codes
  from Telegram's `777000` are never buffered, even with `watch_all`.

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
- [x] 12 core tools (see [Tools](#tools))
- [x] Safety: output sanitizing, chat allowlist, read-only mode, audit log
- [x] Media: `get_media_info`, `download_media` (allowed roots, size limit), `view_photo`
- [x] HTML formatting for sent messages, forum topics
- [x] One-line installer, `install claude-desktop|claude-code`
- [x] Homebrew tap (`brew install itskamol/tap/telegram-mcp`)

## License

Apache-2.0
