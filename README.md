# telegram-mcp (Go)

A Telegram [MCP](https://modelcontextprotocol.io) server shipped as one static
binary: no Python, no `uv`, no Docker. It is a focused Go rewrite inspired by
[chigwell/telegram-mcp](https://github.com/chigwell/telegram-mcp), built on
[gotd/td](https://github.com/gotd/td).

> **Status: early.** Login, secure session storage, 12 core tools and media
> tools work (see [Roadmap](#roadmap)).

## Install

Download the archive for your OS from
[Releases](../../releases), unpack it and put `telegram-mcp` on your `PATH`.

Or build from source (Go 1.26+):

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

Add to `claude_desktop_config.json` (no secrets go here):

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
| `list_chats` | `reply_to_message` |
| `resolve_username` | `forward_message` |
| `search_contacts` | `edit_message` (destructive) |
| `list_messages` | `delete_message` (destructive) |
| `search_messages` | `mark_as_read` |
| `get_media_info` | `download_media` (local disk only) |
| `open_photo` | |

Chats are addressed by the `id` from `list_chats` (Bot API style: users
positive, groups negative, channels `-100…`), by `@username`, or `me`.

## Safety settings

All optional, in `config.toml`:

```toml
read_only = true                         # register only the read tools (also: serve --read-only)
allowed_chat_ids = [123456789, -1001234567890]  # restrict every tool to these chats
max_delete = 20                          # message IDs per delete_message call
allowed_roots = ["~/Downloads/telegram-mcp"]  # where download_media may write (this is the default)
max_download_mb = 50                     # per file
```

- Text from Telegram is returned as JSON with invisible and control characters
  stripped, and tool descriptions tell the model not to follow instructions in it.
- Outside channels Telegram deletes by message ID alone, with no chat attached.
  `delete_message` first checks that every ID belongs to the chat you named
  and refuses otherwise.
- Every write is appended to `~/.local/state/telegram-mcp/audit.log` (tool, chat,
  message IDs, error) without message text.
- `download_media` writes only inside `allowed_roots` (symlinks resolved, `..`
  rejected), reduces file names to one safe component, never overwrites an
  existing file, and checks the size before and during the download.
- `open_photo` shows the model a copy scaled to at most 1280 px (5 MB cap).
- Flood waits up to a minute are absorbed; each tool call times out after 55 s.

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
- [x] Media: `get_media_info`, `download_media` (allowed roots, size limit), `open_photo`
- [ ] Markdown/HTML formatting for sent messages, forum topics
- [ ] `install-claude` helper, Homebrew tap, signed macOS builds

## License

Apache-2.0
