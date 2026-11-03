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
	"time"
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

// MaxSecret bounds a secret's length, and MaxSecretName its name's: on
// macOS the whole command, value included, is one line of security -i,
// which reads at most 4096 bytes a line and runs the rest as another
// command (review 26).
const (
	MaxSecret     = 3 << 10
	MaxSecretName = 64
)

// KeychainTimeout bounds each keychain command. On macOS a keychain can
// stop to ask a person for permission in a window; a worker with nobody
// at the screen gets an error saying so instead of waiting forever.
var KeychainTimeout = 15 * time.Second

// Keychain is the operating system's own vault: the login keychain on
// macOS (through its security command) and the secret service on Linux
// (GNOME Keyring or KWallet, through libsecret's secret-tool). Both
// unlock when you log in to the desktop. A secret is filed as service
// "heedle", account its name, so Keychain Access (or Seahorse) shows and
// deletes it. Like any keychain item, other programs running as you can
// read it; what it keeps secrets from is plans, records, logs, histories
// and the agents' containers, which never see the keychain.
type Keychain struct{}

// CheckSecret reports whether name and value can be kept: a name is an
// environment variable's (the name a connector already uses), at most
// MaxSecretName bytes, and a value is one line of printable ASCII without
// spaces, quotes or backslashes, as tokens and API keys are, at most
// MaxSecret bytes.
func CheckSecret(name, value string) error {
	if !envName.MatchString(name) || len(name) > MaxSecretName {
		return fmt.Errorf("%w: a secret's name is letters, digits and underscores, not starting with a digit, at most %d: %q", ErrSecret, MaxSecretName, name)
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

// Lookup reads name from the keychain. A keychain that is locked, or that
// doesn't answer, is an error, never "not there".
func (k Keychain) Lookup(ctx context.Context, name string) (string, bool, error) {
	if CheckSecret(name, "x") != nil {
		return "", false, nil
	}
	out, errOut, err := run(ctx, keychainCommand("lookup", name), "")
	// No keychain command (a server without secret-tool): nothing there,
	// so the step says how to store it, and storing says what to install.
	if errors.Is(err, exec.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		if gone, gerr := k.absent(ctx, name, err, errOut); gerr != nil || gone {
			return "", false, gerr
		}
		return "", false, keychainError("read", name, err, errOut, "")
	}
	v := strings.TrimSuffix(out, "\n")
	if v == "" {
		return "", false, nil
	}
	if err := CheckSecret(name, v); err != nil {
		return "", false, fmt.Errorf("%w: the keychain's %s isn't one line of printable ASCII (was it made outside heedle?): store it again with `heedle secret set %s`", ErrSetup, name, name)
	}
	return v, true, nil
}

// Store files value under name, replacing what was there. The value goes
// to the keychain command on its standard input, never its arguments,
// so no process listing shows it, and never into an error.
func (Keychain) Store(ctx context.Context, name, value string) error {
	if err := CheckSecret(name, value); err != nil {
		return err
	}
	in := value
	if goos == "darwin" {
		// security reads commands from its standard input with -i; the
		// line stays under its 4096 bytes (MaxSecret, MaxSecretName).
		in = securityLine(name, value)
	}
	_, errOut, err := run(ctx, keychainCommand("store", name), in)
	if err == nil && goos == "darwin" && strings.TrimSpace(errOut) != "" {
		// security -i reports a failed command but still exits 0.
		err = errors.New("security refused it")
	}
	if err != nil {
		return keychainError("store", name, err, errOut, value)
	}
	return nil
}

// Delete removes name; deleting a secret that isn't there is no error,
// but one a locked keychain still holds is.
func (k Keychain) Delete(ctx context.Context, name string) error {
	if err := CheckSecret(name, "x"); err != nil {
		return err
	}
	_, errOut, err := run(ctx, keychainCommand("delete", name), "")
	if err != nil {
		if gone, gerr := k.absent(ctx, name, err, errOut); gerr != nil || gone {
			return gerr
		}
		return keychainError("delete", name, err, errOut, "")
	}
	if goos == "linux" {
		// secret-tool clear succeeds on a locked keyring and leaves the
		// item (review 26).
		if gone, gerr := k.absent(ctx, name, nil, ""); gerr != nil || !gone {
			return keychainError("delete", name, errors.New("it is still there"), "", "")
		}
	}
	return nil
}

// absent reports whether a command that failed with err failed because
// name isn't there. macOS's security says so (exit 44); Linux's
// secret-tool exits 1 saying nothing both for that and for a locked
// keyring, so absent asks secret-tool search, which lists a locked item.
func (Keychain) absent(ctx context.Context, name string, err error, errOut string) (bool, error) {
	var ee *exec.ExitError
	switch goos {
	case "darwin":
		return errors.As(err, &ee) && ee.ExitCode() == 44, nil
	case "linux":
		if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(errOut) == "") {
			return false, nil
		}
		out, errOut, serr := run(ctx, exec.Command("secret-tool", "search", "service", KeychainService, "account", name), "")
		if serr != nil {
			return false, keychainError("read", name, serr, errOut, "")
		}
		if strings.TrimSpace(out+errOut) == "" {
			return true, nil
		}
		return false, fmt.Errorf("%w: keychain: the keyring holding %s is locked: unlock it (log in to the desktop, or run gnome-keyring-daemon --unlock in this session)", ErrSetup, name)
	}
	return false, nil
}

// keychainCommand is the command for op on this system, or nil if this
// system has no keychain pirn knows. Its context is set by run.
func keychainCommand(op, name string) *exec.Cmd {
	switch goos {
	case "darwin":
		switch op {
		case "lookup":
			return exec.Command("security", "find-generic-password", "-s", KeychainService, "-a", name, "-w")
		case "store":
			return exec.Command("security", "-i")
		default:
			return exec.Command("security", "delete-generic-password", "-s", KeychainService, "-a", name)
		}
	case "linux":
		switch op {
		case "lookup":
			return exec.Command("secret-tool", "lookup", "service", KeychainService, "account", name)
		case "store":
			return exec.Command("secret-tool", "store", "--label=heedle "+name, "service", KeychainService, "account", name)
		default:
			return exec.Command("secret-tool", "clear", "service", KeychainService, "account", name)
		}
	}
	return nil
}

// run runs a keychain command with in on its standard input, for at
// most KeychainTimeout.
func run(ctx context.Context, cmd *exec.Cmd, in string) (stdout, stderr string, err error) {
	if cmd == nil {
		return "", "", errNoKeychain()
	}
	if cmd.Err != nil {
		return "", "", cmd.Err
	}
	ctx, cancel := context.WithTimeout(ctx, KeychainTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	// A child that keeps the pipes open mustn't outlast the deadline.
	c.WaitDelay = time.Second
	var out, errb bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = strings.NewReader(in), &out, &errb
	err = c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%w: keychain: %s didn't answer in %v: is it waiting for you to allow access in a window, or locked?", ErrSetup, cmd.Args[0], KeychainTimeout)
	}
	return out.String(), errb.String(), err
}

func errNoKeychain() error {
	return fmt.Errorf("%w: this system has no keychain heedle can use (macOS or Linux only)", ErrSetup)
}

// keychainError explains a failed keychain command. What the command
// said is kept, unless it holds the value being stored.
func keychainError(op, name string, err error, stderr, value string) error {
	if errors.Is(err, ErrSetup) {
		return err
	}
	hint := ""
	if goos == "linux" {
		if errors.Is(err, exec.ErrNotFound) {
			hint = " (install secret-tool: libsecret-tools on Debian and Ubuntu, libsecret on Fedora)"
		} else {
			hint = " (is a secret service such as GNOME Keyring running and unlocked in this session?)"
		}
	}
	msg := strings.TrimSpace(stderr)
	if value != "" && strings.Contains(msg, value[:min(len(value), 8)]) {
		msg = "(its message is left out: it may hold part of the value)"
	}
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
	return "", fmt.Errorf("%w: connector %q needs %s: store it with `heedle secret set %s` (or `loom secret set %s`), or set it where the worker runs", ErrSetup, connector, name, name, name)
}

// LookupSecret is name's value from the environment, else from v, else
// "": for a key a worker can start without, such as ANTHROPIC_API_KEY.
// A vault that couldn't be read is returned as the error, for the caller
// to report; the value is then "".
func LookupSecret(ctx context.Context, v Vault, name string) (string, error) {
	if s := getenv(name); s != "" || v == nil {
		return s, nil
	}
	s, _, err := v.Lookup(ctx, name)
	return s, err
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

// securityLine is the security -i command that stores value as name.
func securityLine(name, value string) string {
	return fmt.Sprintf("add-generic-password -U -s %s -a %s -l \"heedle %s\" -w \"%s\"\n", KeychainService, name, name, value)
}
