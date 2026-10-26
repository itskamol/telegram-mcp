// Command telegram-mcp is a single-binary Telegram MCP server.
//
//	telegram-mcp login [--qr] [--phone +998...] [--account name]
//	telegram-mcp status [--account name]
//	telegram-mcp logout [--account name]
//	telegram-mcp version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/config"
	"github.com/itskamol/telegram-mcp-go/internal/login"
	"github.com/itskamol/telegram-mcp-go/internal/secret"
	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

// version is set by the release build: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `telegram-mcp: Telegram MCP server in one binary

Usage:
  telegram-mcp login  [--qr] [--phone +998...] [--account name]
  telegram-mcp status [--account name]
  telegram-mcp logout [--account name]
  telegram-mcp version

Settings live in config.toml in the profile directory (see "status").
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

func (e *env) close() { _ = e.db.Close() }

// setup parses the shared --account flag, loads config and opens storage.
// needAPI is false for commands that can run without API credentials.
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
	cfg, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	if needAPI {
		if err := cfg.Validate(paths); err != nil {
			return nil, err
		}
	}
	if err := paths.EnsureDir(); err != nil {
		return nil, err
	}
	db, err := storage.Open(paths.DB)
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

func (e *env) client(handler telegram.UpdateHandler) *telegram.Client {
	return telegram.NewClient(e.cfg.APIID, e.cfg.APIHash, telegram.Options{
		SessionStorage: e.session,
		UpdateHandler:  handler,
		Device: telegram.DeviceConfig{
			DeviceModel: "telegram-mcp",
			AppVersion:  version,
		},
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
	if err != nil {
		return err
	}
	defer e.close()

	fmt.Printf("profile:  %s\naccount:  %s\n", e.paths.Dir, e.account)
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
