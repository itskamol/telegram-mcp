# telegram-mcp (Go)

- Never commit `.env`, `config.toml` or `*.db`, and never print the session, `api_hash` or keychain key in output or logs.
- Live tests against Telegram run only in Saved Messages or a dedicated test group — never send, edit, forward or delete in real chats.
- One process per profile: two connections on one auth key make Telegram return `AUTH_KEY_DUPLICATED` and drop both. Extra `serve` processes share one connection (see `internal/hub`).
- The tool set is deliberately small — the essentials, not the upstream Python project's full list. Ask before adding a tool.
- Safety behaviour (allowlist, read-only, delete check, `allowed_roots`, audit log) is documented in README "Safety settings"; update README when it changes.
- CI runs `go vet ./...` and `go test -race ./...`; run both before calling work done.
