// Command telegram-mcp is a single-binary Telegram MCP server.
//
//	telegram-mcp serve [--read-only] [--account name]
//	telegram-mcp login [--qr] [--phone +998...] [--account name]
//	telegram-mcp status [--account name]
//	telegram-mcp logout [--account name]
//	telegram-mcp install claude-desktop|claude-code [--name telegram] [--read-only] [--force]
//	telegram-mcp version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/config"
	"github.com/itskamol/telegram-mcp-go/internal/hub"
	"github.com/itskamol/telegram-mcp-go/internal/install"
	"github.com/itskamol/telegram-mcp-go/internal/login"
	"github.com/itskamol/telegram-mcp-go/internal/mcpserver"
	"github.com/itskamol/telegram-mcp-go/internal/secret"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

// version is set by the release build: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `telegram-mcp: Telegram MCP server in one binary

Usage:
  telegram-mcp serve  [--read-only] [--account name]   run the MCP server on stdio
  telegram-mcp login  [--qr] [--phone +998...] [--account name]
  telegram-mcp status [--account name]
  telegram-mcp logout [--account name]
  telegram-mcp install claude-desktop|claude-code [--name telegram] [--read-only] [--force]
  telegram-mcp version

Settings live in ~/.config/telegram-mcp/config.toml (see "status").
The session is stored encrypted in SQLite; its key is kept in the OS keychain.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = runServe(ctx, args)
	case "install":
		err = runInstall(ctx, args)
	case "login":
		err = runLogin(ctx, args)
	case "status":
		err = runStatus(ctx, args)
	case "logout":
		err = runLogout(ctx, args)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// env is everything a command needs once flags are parsed.
type env struct {
	paths   config.Paths
	cfg     config.Config
	db      *storage.DB
	session *storage.Session
	account string
}

func (e *env) close() {
	if e.db != nil {
		_ = e.db.Close()
	}
}

// setup parses the shared --account flag, loads config and opens storage.
// needAPI is false for commands that can run without API credentials.
// When another process holds the profile it returns storage.ErrLocked
// together with an env that has paths, config and account but no database.
func setup(fs *flag.FlagSet, args []string, needAPI bool) (*env, error) {
	account := fs.String("account", config.DefaultAccount, "account label, for several Telegram accounts")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := config.ValidateAccount(*account); err != nil {
		return nil, err
	}

	paths, err := config.ResolvePaths()
	if err != nil {
		return nil, err
	}
	moved, err := config.Migrate(config.LegacyDir(), paths)
	if err != nil {
		return nil, fmt.Errorf("move profile to the XDG layout: %w", err)
	}
	for _, m := range moved {
		fmt.Fprintf(os.Stderr, "moved to %s\n", m)
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	if needAPI {
		if err := cfg.Validate(paths); err != nil {
			return nil, err
		}
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	db, err := storage.Open(paths.DB)
	if errors.Is(err, storage.ErrLocked) {
		return &env{paths: paths, cfg: cfg, account: *account}, err
	}
	if err != nil {
		return nil, err
	}
	return &env{
		paths:   paths,
		cfg:     cfg,
		db:      db,
		session: storage.NewSession(db, secret.Keyring{}, *account),
		account: *account,
	}, nil
}

func (e *env) client(handler telegram.UpdateHandler, mw ...telegram.Middleware) *telegram.Client {
	return telegram.NewClient(e.cfg.APIID, e.cfg.APIHash, telegram.Options{
		SessionStorage: e.session,
		UpdateHandler:  handler,
		NoUpdates:      handler == nil,
		Middlewares:    mw,
		Device: telegram.DeviceConfig{
			DeviceModel: "telegram-mcp",
			AppVersion:  version,
		},
	})
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	readOnly := fs.Bool("read-only", false, "register only the read tools (overrides config)")
	e, err := setup(fs, args, true)
	if errors.Is(err, storage.ErrLocked) {
		// Another serve owns the Telegram connection: share it.
		ro := *readOnly || e.cfg.ReadOnly
		return hub.Relay(ctx, hub.SocketPath(e.paths.DataDir, e.account, version, ro), os.Stdin, os.Stdout, 30*time.Second)
	}
	if err != nil {
		return err
	}
	defer e.close()

	ok, err := e.session.Exists(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not logged in: run `telegram-mcp login` first")
	}

	auditLog, err := audit.Open(e.paths.Audit, e.account)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	// Flood waits up to a minute are slept through transparently; longer
	// ones fail the tool call instead of hanging past its timeout.
	waiter := floodwait.NewWaiter().WithMaxWait(time.Minute).WithMaxRetries(3)
	c := e.client(nil, waiter)

	return waiter.Run(ctx, func(ctx context.Context) error {
		return c.Run(ctx, func(ctx context.Context) error {
			self, err := c.Self(ctx)
			if err != nil {
				return fmt.Errorf("session is not authorized (run `telegram-mcp login`): %w", err)
			}
			svc, err := tele.NewService(ctx, c.API(), storage.NewPeers(e.db, e.account), self)
			if err != nil {
				return err
			}
			policy := mcpserver.Policy{
				ReadOnly:       e.cfg.ReadOnly,
				AllowedChatIDs: e.cfg.AllowedChatIDs,
				MaxDelete:      e.cfg.MaxDelete,
				AllowedRoots:   e.cfg.AllowedRoots,
				MaxDownload:    int64(e.cfg.MaxDownloadMB) << 20,
				MaxUpload:      int64(e.cfg.MaxUploadMB) << 20,
			}
			full := mcpserver.New(svc, policy, auditLog, version)
			policy.ReadOnly = true
			readOnlySrv := mcpserver.New(svc, policy, auditLog, version)

			// Serve other processes of this profile on two sockets, one per
			// mode, so a --read-only relay can only reach read tools.
			ctx, cancel := context.WithCancel(ctx)
			var wg sync.WaitGroup
			defer func() {
				cancel()  // stops the listeners and relayed sessions...
				wg.Wait() // ...and waits for them, in this order
			}()
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			for ro, srv := range map[bool]*mcp.Server{false: full, true: readOnlySrv} {
				ln, err := hub.Listen(hub.SocketPath(e.paths.DataDir, e.account, version, ro))
				if err != nil {
					return fmt.Errorf("listen for other processes: %w", err)
				}
				wg.Go(func() { hub.Serve(ctx, ln, srv, log) })
			}

			own := full
			if *readOnly {
				own = readOnlySrv
			}
			fmt.Fprintf(os.Stderr, "telegram-mcp %s serving %s on stdio\n", version, describe(self))
			return own.Run(ctx, &mcp.StdioTransport{})
		})
	})
}

func runLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	useQR := fs.Bool("qr", false, "log in by scanning a QR code instead of entering a code")
	phone := fs.String("phone", "", "phone number in international format (asked if empty)")
	e, err := setup(fs, args, true)
	if err != nil {
		return err
	}
	defer e.close()

	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(dispatcher)
	c := e.client(dispatcher)
	p := login.NewPrompter()

	return c.Run(ctx, func(ctx context.Context) error {
		if *useQR {
			err = login.QR(ctx, c, p, loggedIn)
		} else {
			err = login.Phone(ctx, c, p, *phone)
		}
		if err != nil {
			return fmt.Errorf("login: %w", err)
		}
		me, err := c.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Logged in as %s (account %q).\n", describe(me), e.account)
		return nil
	})
}

func runStatus(ctx context.Context, args []string) error {
	e, err := setup(flag.NewFlagSet("status", flag.ContinueOnError), args, false)
	locked := errors.Is(err, storage.ErrLocked)
	if err != nil && !locked {
		return err
	}
	defer e.close()

	fmt.Printf("config:   %s\ndata:     %s\naudit:    %s\naccount:  %s\n",
		e.paths.Config, e.paths.DB, e.paths.Audit, e.account)
	if locked {
		// Connecting now would open a second connection on the same auth key.
		fmt.Println("session:  in use by a running telegram-mcp server (e.g. in Claude Desktop); not checked")
		return nil
	}
	ok, err := e.session.Exists(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("session:  none (run `telegram-mcp login`)")
		return nil
	}
	if err := e.cfg.Validate(e.paths); err != nil {
		return err
	}

	c := e.client(nil)
	return c.Run(ctx, func(ctx context.Context) error {
		st, err := c.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !st.Authorized {
			fmt.Println("session:  stored but not authorized (run `telegram-mcp login`)")
			return nil
		}
		fmt.Printf("session:  authorized as %s\n", describe(st.User))
		return nil
	})
}

func runLogout(ctx context.Context, args []string) error {
	e, err := setup(flag.NewFlagSet("logout", flag.ContinueOnError), args, false)
	if err != nil {
		return err
	}
	defer e.close()

	ok, err := e.session.Exists(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "Not logged in.")
		return e.session.Delete(ctx) // drop a stray keychain key, if any
	}

	// Revoke on Telegram's side first, so a copied database is useless.
	// A local wipe still happens if that fails (offline, key already gone).
	var remoteErr error
	if e.cfg.Validate(e.paths) == nil {
		c := e.client(nil)
		remoteErr = c.Run(ctx, func(ctx context.Context) error {
			_, err := c.API().AuthLogOut(ctx)
			return err
		})
	} else {
		remoteErr = errors.New("api_id/api_hash not configured")
	}

	if err := e.session.Delete(ctx); err != nil {
		return err
	}
	if remoteErr != nil {
		fmt.Fprintf(os.Stderr, "Local session deleted, but Telegram was not told (%v).\n"+
			"End it manually in Telegram > Settings > Devices.\n", remoteErr)
		return nil
	}
	fmt.Fprintln(os.Stderr, "Logged out and local session deleted.")
	return nil
}

func describe(u *tg.User) string {
	if u == nil {
		return "unknown user"
	}
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	if u.Username != "" {
		name += " @" + u.Username
	}
	return fmt.Sprintf("%s (id %d)", name, u.ID)
}

func runInstall(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "claude-desktop" && args[0] != "claude-code") {
		return errors.New("usage: telegram-mcp install claude-desktop|claude-code [--name telegram] [--read-only] [--force] [--account name]")
	}
	target := args[0]
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	name := fs.String("name", "telegram", "MCP server name in the client")
	readOnly := fs.Bool("read-only", false, "serve only the read tools")
	force := fs.Bool("force", false, "replace an existing entry with the same name")
	account := fs.String("account", config.DefaultAccount, "account label")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := config.ValidateAccount(*account); err != nil {
		return err
	}
	exe, err := install.Executable()
	if err != nil {
		return err
	}
	serveArgs := []string{"serve"}
	if *readOnly {
		serveArgs = append(serveArgs, "--read-only")
	}
	if *account != config.DefaultAccount {
		serveArgs = append(serveArgs, "--account", *account)
	}

	if target == "claude-code" {
		cmdArgs := append([]string{"mcp", "add", "--scope", "user", *name, "--", exe}, serveArgs...)
		claude, err := exec.LookPath("claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude CLI not found on PATH. Run this yourself:\n  claude %s\n", strings.Join(cmdArgs, " "))
			return nil
		}
		cmd := exec.CommandContext(ctx, claude, cmdArgs...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}

	path, err := install.DesktopConfigPath()
	if err != nil {
		return err
	}
	changed, err := install.AddToDesktopConfig(path, *name, install.Server{Command: exe, Args: serveArgs}, *force)
	if errors.Is(err, install.ErrExists) {
		return fmt.Errorf("%q is already in %s with other settings; pick another --name or pass --force", *name, path)
	}
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(os.Stderr, "Already installed as %q in %s.\n", *name, path)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Added %q to %s (previous version kept as .bak).\nRestart Claude Desktop to load it.\n", *name, path)
	return nil
}
