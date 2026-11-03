package pirn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// A Vault holds a worker's secrets by name: connectors' tokens and keys,
// so they need not sit in the worker's environment. Lookup reports
// whether the vault has name; an error means the vault couldn't be asked.
type Vault interface {
	Lookup(ctx context.Context, name string) (value string, ok bool, err error)
}

// goos is runtime.GOOS; a variable so tests can try both systems.
var goos = runtime.GOOS

// KeychainService is the service every heedle secret is filed under in
// the operating system's keychain.
const KeychainService = "heedle"

// ErrSecret is a secret's name or value pirn won't keep.
var ErrSecret = errors.New("pirn: secret refused")

// MaxSecret bounds a secret's length.
const MaxSecret = 8 << 10

// Keychain is the operating system's own vault: the login keychain on
// macOS (through its security command) and the secret service on Linux
// (GNOME Keyring or KWallet, through libsecret's secret-tool). Both
// unlock when you log in. A secret is filed as service "heedle", account
// its name, so Keychain Access (or Seahorse) shows and deletes it.
type Keychain struct{}

// CheckSecret reports whether name and value can be kept: a name is an
// environment variable's (the name a connector already uses), and a
// value is one line of printable ASCII without quotes or backslashes, as
// tokens and API keys are, at most MaxSecret bytes.
func CheckSecret(name, value string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("%w: a secret's name is letters, digits and underscores, not starting with a digit: %q", ErrSecret, name)
	}
	if value == "" || len(value) > MaxSecret {
		return fmt.Errorf("%w: %s: a value is 1 to %d bytes", ErrSecret, name, MaxSecret)
	}
	for _, r := range value {
		if r <= ' ' || r > '~' || r == '"' || r == '\\' {
			return fmt.Errorf("%w: %s: a value is printable ASCII with no spaces, quotes or backslashes", ErrSecret, name)
		}
	}
	return nil
}

// Lookup reads name from the keychain.
func (Keychain) Lookup(ctx context.Context, name string) (string, bool, error) {
	if !envName.MatchString(name) {
		return "", false, nil
	}
	var out, errb bytes.Buffer
	cmd, missing := keychainCommand(ctx, "lookup", name)
	if cmd == nil {
		return "", false, errNoKeychain()
	}
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	// No keychain command (a server without secret-tool): nothing there,
	// so the step says how to store it, and storing says what to install.
	if errors.Is(err, exec.ErrNotFound) {
		return "", false, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && missing(ee.ExitCode(), errb.String()) {
		return "", false, nil
	}
	if err != nil {
		return "", false, keychainError("read", name, err, errb.String())
	}
	v := strings.TrimSuffix(out.String(), "\n")
	return v, v != "", nil
}

// Store files value under name, replacing what was there. The value goes
// to the keychain command on its standard input, never its arguments,
// so no process listing shows it.
func (Keychain) Store(ctx context.Context, name, value string) error {
	if err := CheckSecret(name, value); err != nil {
		return err
	}
	cmd, _ := keychainCommand(ctx, "store", name)
	if cmd == nil {
		return errNoKeychain()
	}
	if goos == "darwin" {
		// security reads commands from its standard input with -i.
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -l \"heedle %s\" -w \"%s\"\n", KeychainService, name, name, value))
	} else {
		cmd.Stdin = strings.NewReader(value)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return keychainError("store", name, err, errb.String())
	}
	// security -i reports a failed command but still exits 0.
	if s := strings.TrimSpace(errb.String()); s != "" && goos == "darwin" {
		return keychainError("store", name, errors.New("security refused it"), s)
	}
	return nil
}

// Delete removes name; deleting a secret that isn't there is no error.
func (Keychain) Delete(ctx context.Context, name string) error {
	if !envName.MatchString(name) {
		return CheckSecret(name, "x")
	}
	cmd, missing := keychainCommand(ctx, "delete", name)
	if cmd == nil {
		return errNoKeychain()
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && missing(ee.ExitCode(), errb.String()) {
		return nil
	}
	if err != nil {
		return keychainError("delete", name, err, errb.String())
	}
	return nil
}

// keychainCommand is the command for op on this system, and how to tell
// "not there" from a failure; nil if this system has no keychain pirn
// knows.
func keychainCommand(ctx context.Context, op, name string) (*exec.Cmd, func(code int, stderr string) bool) {
	switch goos {
	case "darwin":
		// security exits 44 for an item it can't find.
		missing := func(code int, _ string) bool { return code == 44 }
		switch op {
		case "lookup":
			return exec.CommandContext(ctx, "security", "find-generic-password", "-s", KeychainService, "-a", name, "-w"), missing
		case "store":
			return exec.CommandContext(ctx, "security", "-i"), missing
		default:
			return exec.CommandContext(ctx, "security", "delete-generic-password", "-s", KeychainService, "-a", name), missing
		}
	case "linux":
		// secret-tool exits 1, saying nothing, for an item it can't find.
		missing := func(code int, stderr string) bool { return code == 1 && strings.TrimSpace(stderr) == "" }
		switch op {
		case "lookup":
			return exec.CommandContext(ctx, "secret-tool", "lookup", "service", KeychainService, "account", name), missing
		case "store":
			return exec.CommandContext(ctx, "secret-tool", "store", "--label=heedle "+name, "service", KeychainService, "account", name), missing
		default:
			return exec.CommandContext(ctx, "secret-tool", "clear", "service", KeychainService, "account", name), missing
		}
	}
	return nil, nil
}

func errNoKeychain() error {
	return fmt.Errorf("%w: this system has no keychain heedle can use (macOS or Linux only)", ErrSetup)
}

func keychainError(op, name string, err error, stderr string) error {
	hint := ""
	if goos == "linux" {
		var ee *exec.Error
		if errors.As(err, &ee) {
			hint = " (install secret-tool: libsecret-tools on Debian and Ubuntu, libsecret on Fedora)"
		} else {
			hint = " (is a secret service such as GNOME Keyring running and unlocked in this session?)"
		}
	}
	msg := strings.TrimSpace(stderr)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("%w: keychain: couldn't %s %s: %v %s%s", ErrSetup, op, name, err, msg, hint)
}

// secretFor is name's value for a connector: the worker's environment
// first (so CI and anyone who prefers variables keep working), then the
// vault.
func secretFor(ctx context.Context, v Vault, connector, name string) (string, error) {
	if s := getenv(name); s != "" {
		return s, nil
	}
	if v != nil {
		s, ok, err := v.Lookup(ctx, name)
		if err != nil {
			return "", err
		}
		if ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: connector %q needs %s: store it with `heedle secret set %s`, or set it where the worker runs", ErrSetup, connector, name, name)
}

// LookupSecret is name's value from the environment, else from v, else
// "": for a key a worker can start without, such as ANTHROPIC_API_KEY,
// whose steps then fail with the API's own refusal.
func LookupSecret(ctx context.Context, v Vault, name string) string {
	if s := getenv(name); s != "" || v == nil {
		return s
	}
	s, _, _ := v.Lookup(ctx, name)
	return s
}

// NoEcho turns off echo on the terminal at standard input (stty -echo),
// for typing a secret, and returns what turns it back on.
func NoEcho(ctx context.Context) (restore func(), err error) {
	stty := func(ctx context.Context, arg string) error {
		cmd := exec.CommandContext(ctx, "stty", arg)
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
	if err := stty(ctx, "-echo"); err != nil {
		return nil, err
	}
	return func() { stty(context.WithoutCancel(ctx), "echo") }, nil
}
