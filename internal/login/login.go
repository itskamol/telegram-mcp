// Package login runs the interactive sign-in flows on the terminal.
//
// Prompts go to stderr so stdout stays free; the MCP server later owns stdout
// for the protocol and must never share it with human-facing text.
package login

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"
)

// errSignUp is returned for numbers without a Telegram account: creating
// accounts is out of scope, the official app does that.
var errSignUp = errors.New("this phone number has no Telegram account; sign up in an official Telegram app first")

// Prompter reads answers from the terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// NewPrompter reads from stdin and writes prompts to stderr.
func NewPrompter() *Prompter {
	return &Prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr}
}

func (p *Prompter) line(label string) (string, error) {
	fmt.Fprint(p.out, label)
	s, err := p.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// secret reads without echo when stdin is a terminal.
func (p *Prompter) secret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return p.line(label)
	}
	fmt.Fprint(p.out, label)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(p.out)
	return strings.TrimSpace(string(b)), err
}

// codeAuth adapts Prompter to gotd's UserAuthenticator.
type codeAuth struct {
	p     *Prompter
	phone string
}

func (a codeAuth) Phone(context.Context) (string, error) {
	if a.phone != "" {
		return a.phone, nil
	}
	return a.p.line("Phone number (international format, e.g. +998901234567): ")
}

func (a codeAuth) Code(_ context.Context, sent *tg.AuthSentCode) (string, error) {
	via := "Telegram"
	if t, ok := sent.Type.(*tg.AuthSentCodeTypeSMS); ok && t != nil {
		via = "SMS"
	}
	return a.p.line(fmt.Sprintf("Login code (sent via %s): ", via))
}

func (a codeAuth) Password(context.Context) (string, error) {
	return a.p.secret("2FA password: ")
}

func (codeAuth) SignUp(context.Context) (auth.UserInfo, error) { return auth.UserInfo{}, errSignUp }

func (codeAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errSignUp
}

// Phone signs in with phone number, login code and, if enabled, the 2FA
// password. It is a no-op when the session is already authorized.
func Phone(ctx context.Context, c *telegram.Client, p *Prompter, phone string) error {
	flow := auth.NewFlow(codeAuth{p: p, phone: phone}, auth.SendCodeOptions{})
	return c.Auth().IfNecessary(ctx, flow)
}

// QR shows a login QR code to scan from Telegram > Settings > Devices.
// loggedIn must come from qrlogin.OnLoginToken on the client's dispatcher.
func QR(ctx context.Context, c *telegram.Client, p *Prompter, loggedIn qrlogin.LoggedIn) error {
	st, err := c.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if st.Authorized {
		return nil
	}

	show := func(_ context.Context, t qrlogin.Token) error {
		fmt.Fprintln(p.out, "\nScan with Telegram > Settings > Devices > Link Desktop Device:")
		qrterminal.GenerateHalfBlock(t.URL(), qrterminal.L, p.out)
		fmt.Fprintf(p.out, "Expires at %s; a new code is shown automatically.\n", t.Expires().Format("15:04:05"))
		return nil
	}
	_, err = c.QR().Auth(ctx, loggedIn, show)
	if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		return err
	}

	pwd, err := p.secret("2FA password: ")
	if err != nil {
		return err
	}
	_, err = c.Auth().Password(ctx, pwd)
	return err
}
