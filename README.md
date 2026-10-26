# telegram-mcp (Go)

A Telegram [MCP](https://modelcontextprotocol.io) server shipped as one static
binary: no Python, no `uv`, no Docker. It is a focused Go rewrite inspired by
[chigwell/telegram-mcp](https://github.com/chigwell/telegram-mcp), built on
[gotd/td](https://github.com/gotd/td).

> **Status: early.** Login and secure session storage work. The MCP server and
> its tools are next (see [Roadmap](#roadmap)).

## Install

Download the archive for your OS from
[Releases](../../releases), unpack it and put `telegram-mcp` on your `PATH`.

Or build from source (Go 1.26+):

```bash
go install github.com/itskamol/telegram-mcp-go/cmd/telegram-mcp@latest
```

## Set up

1. Create an app at <https://my.telegram.org/apps> to get `api_id` and `api_hash`.
2. Run `telegram-mcp status` once to see your profile directory, then create
   `config.toml` there:

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

## How the session is stored

| What | Where |
|---|---|
| Session (auth key) | SQLite, encrypted with AES-256-GCM |
| Encryption key | OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service) |
| Peer cache | SQLite |
| `api_id`, `api_hash` | `config.toml` |

- The session never sits in plain text: not in `.env`, not in the MCP client
  config, not in the database.
- The profile directory is `0700` and the database `0600`.
- A file lock lets only one process use a profile. Two connections on one auth
  key make Telegram return `AUTH_KEY_DUPLICATED` and drop both.
- A session row is bound to its account name, so it cannot be copied under
  another account.

## Roadmap

- [x] Login (phone, QR, 2FA), status, logout
- [x] Encrypted SQLite session, keychain key, single-process lock
- [ ] MCP server over stdio with flood-wait handling and per-tool timeouts
- [ ] Tools: `list_chats`, `resolve_username`, `search_contacts`, `list_messages`,
      `search_messages`, `get_me`, `send_message`, `reply_to_message`,
      `forward_message`, `edit_message`, `delete_message`, `mark_as_read`
- [ ] Media: `get_media_info`, `download_media` (allowed roots, size limit), `open_photo`
- [ ] Safety: output sanitizing, chat allowlist, `--read-only`, audit log
- [ ] `install-claude` helper, Homebrew tap, signed macOS builds

## License

Apache-2.0
