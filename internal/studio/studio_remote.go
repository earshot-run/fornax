package studio

// `fornax studio -on host` runs the studio on another machine — a GPU box at
// home, a rented cloud GPU — and drives it from this machine's browser. The
// remote studio still binds its own loopback; an ssh -L forward on the same
// port number is the only way in, so the remote guard's Host check and the
// link it prints both work here unchanged.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/ui"
)

// Local tunnel ports: clear of the built-ins (7341–7354) and the customs
// (7401+), so a tunnel never takes a port a model expects.
const (
	RemotePortBase      = 7360
	studioRemotePortEnd = 7400
)

// A leashed studio that finds its port taken by something other than a
// studio exits with this, so the local side knows to retry on the next port.
const studioLeashBusy = 75

// The link is printed before any engine work; 60 s covers a slow login and
// a cold disk, and leaves room for a password or host-key prompt.
const studioLinkTimeout = 60 * time.Second

const installScriptURL = "https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh"

var (
	errTunnelPortBusy = errors.New("tunnel port busy")
	errFornaxMissing  = errors.New("fornax isn't installed")
)

// Run the studio on host over ssh and drive it from here, trying tunnel
// ports upward from port until one is free on both ends. A remote without
// fornax gets offered an install of release tag (latest when empty).
func On(ctx context.Context, host, fornaxPath string, port int, noOpen bool, tag string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("-on takes [user@]host or an ssh-config alias, not %q", host)
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("-on needs an ssh client on PATH — install OpenSSH")
	}
	offered := false
	for range 5 {
		port, err = freeTunnelPort(port)
		if err != nil {
			return err
		}
		err = runTunnel(ctx, sshPath, host, fornaxPath, port, noOpen)
		if errors.Is(err, errFornaxMissing) && !offered && fornaxPath == "fornax" && ui.IsTTY(os.Stdin) {
			offered = true
			if !confirmRemoteInstall(host, tag) {
				return err
			}
			if err := installRemote(ctx, sshPath, host, tag); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, errTunnelPortBusy) {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("port %d is taken on one end — trying the next", port)))
		port++
	}
	return errors.New("every tunnel port tried was taken — pass -port to start somewhere else")
}

// The first port at or above from that is free on this machine's loopback.
func freeTunnelPort(from int) (int, error) {
	for port := from; port < studioRemotePortEnd || port == from; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free tunnel port in %d–%d — pass -port", from, studioRemotePortEnd-1)
}

// The ssh command line: forward the same port on both ends, give up if the
// forward can't be set up, notice a dead link within 45 s, and run the
// studio remotely on the leash.
func sshStudioArgs(host, fornaxPath string, port int) []string {
	return []string{
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port, port),
		"--", host,
		remoteStudioCommand(fornaxPath, port),
	}
}

// sshd hands the command to the login shell, which may be bash, zsh or fish,
// so everything runs under an explicit sh. Non-interactive shells often skip
// the profile that puts ~/.local/bin (install.sh's default) and ~/go/bin
// (go install's) on PATH, so those are added here.
func remoteStudioCommand(fornaxPath string, port int) string {
	script := fmt.Sprintf(`PATH="$HOME/.local/bin:$HOME/go/bin:$PATH"; export PATH; exec %s studio -no-open -port %d -leash`,
		remoteWord(fornaxPath), port)
	return "sh -c " + shQuote(script)
}

// A path for the remote sh: ~/ expands to the remote $HOME, the rest is
// quoted literally.
func remoteWord(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return `"$HOME"/` + shQuote(rest)
	}
	return shQuote(path)
}

var shSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// POSIX single-quoting. fish and csh read the close-escape-reopen idiom for
// an embedded quote the same way sh does.
func shQuote(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runTunnel(ctx context.Context, sshPath, host, fornaxPath string, port int, noOpen bool) error {
	tunnelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	remote := newRemoteLog(host, os.Stderr)
	stdoutR, stdoutW := io.Pipe()
	// ssh runs with the caller's environment: it needs SSH_AUTH_SOCK, the
	// user's config and agent, and it runs nothing of ours on this machine.
	// The scrubbed environment is for model runtimes.
	cmd := exec.CommandContext(tunnelCtx, sshPath, sshStudioArgs(host, fornaxPath, port)...)
	cmd.Stdout = stdoutW
	cmd.Stderr = remote
	cmd.WaitDelay = 2 * time.Second
	// Held open and never written: when this process goes, ssh sees EOF and
	// the remote studio's leash ends it. (-n would end it at once.)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("connecting to %s…", host)))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ssh: %w", err)
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		stdoutW.Close()
		exited <- err
	}()

	stdout := bufio.NewReader(stdoutR)
	link, err := readStudioLink(stdout, port, studioLinkTimeout, remote.line)
	if err != nil {
		cancel()
		waitErr := <-exited
		remote.flush()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errNoStudioLink) {
			return tunnelFailure(host, exitCode(waitErr), remote.last())
		}
		return fmt.Errorf("no studio link from %s after %s — see its output above", host, studioLinkTimeout)
	}
	go io.Copy(io.Discard, stdout)

	fmt.Fprintf(os.Stderr, "%s studio on %s through %s — Ctrl-C stops\n",
		ui.Green("✓"), ui.Bold(host), ui.Bold(fmt.Sprintf("http://127.0.0.1:%d", port)))
	fmt.Println(link)
	if !noOpen {
		openBrowser(link)
	}
	waitErr := <-exited
	remote.flush()
	if ctx.Err() != nil {
		return nil
	}
	msg := fmt.Sprintf("lost the studio on %s", host)
	if code := exitCode(waitErr); code == 255 {
		msg = fmt.Sprintf("the connection to %s dropped", host)
	} else if code > 0 {
		msg += fmt.Sprintf(" (exit %d)", code)
	}
	if last := lastLine(remote.last()); last != "" {
		msg += ": " + last
	}
	return errors.New(msg)
}

var (
	errNoStudioLink   = errors.New("the remote exited before printing a studio link")
	errStudioLinkLate = errors.New("no studio link in time")
)

// Waits for the remote studio's link: a line starting
// http://127.0.0.1:<port>/?key=. Anything else a chatty remote profile puts
// on stdout goes to other. EOF first is errNoStudioLink.
func readStudioLink(r *bufio.Reader, port int, timeout time.Duration, other func(string)) (string, error) {
	prefix := fmt.Sprintf("http://127.0.0.1:%d/?key=", port)
	found := make(chan string, 1)
	go func() {
		defer close(found)
		for {
			line, err := r.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, prefix) && len(line) > len(prefix) {
				found <- line
				return
			}
			if line != "" && other != nil {
				other(line)
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case link, ok := <-found:
		if !ok {
			return "", errNoStudioLink
		}
		return link, nil
	case <-timer.C:
		return "", errStudioLinkLate
	}
}

// Why ssh ended before a link, in words the user can act on. code is ssh's
// exit status: 255 is ssh itself, anything else is the remote command's.
func tunnelFailure(host string, code int, lines []string) error {
	text := strings.Join(lines, "\n")
	last := lastLine(lines)
	with := func(msg string) error {
		if last != "" {
			return fmt.Errorf("%s: %s", msg, last)
		}
		return errors.New(msg)
	}
	switch {
	case code == studioLeashBusy:
		return errTunnelPortBusy
	case code == 255 && strings.Contains(text, "forwarding") &&
		(strings.Contains(text, "in use") || strings.Contains(text, "cannot listen") || strings.Contains(text, "bind")):
		return errTunnelPortBusy
	case strings.Contains(text, "is not recognized") || strings.Contains(text, "not recognized as"):
		return fmt.Errorf("%s doesn't run a POSIX sh — the studio's remote must be Linux or macOS", host)
	case code == 255 && strings.Contains(text, "Permission denied"):
		return fmt.Errorf("ssh to %s was refused (Permission denied) — check your key, agent or ~/.ssh/config", host)
	case code == 255 && strings.Contains(text, "Host key verification failed"):
		return fmt.Errorf("ssh doesn't trust %s's host key — connect once with `ssh %s` to check and accept it", host, host)
	case code == 255:
		return with(fmt.Sprintf("couldn't reach %s over ssh", host))
	case code == 127:
		return fmt.Errorf("%w on %s (looked on PATH, ~/.local/bin and ~/go/bin) — install it there with\n  curl -fsSL %s | sh\nor point -fornax at it", errFornaxMissing, host, installScriptURL)
	case code == 2 && strings.Contains(text, "-leash"):
		return fmt.Errorf("fornax on %s is too old for -on — run `fornax upgrade` there", host)
	case code < 0:
		return with(fmt.Sprintf("ssh to %s ended before the studio was ready", host))
	}
	return with(fmt.Sprintf("the studio on %s exited before it was ready (exit %d)", host, code))
}

func confirmRemoteInstall(host, tag string) bool {
	release := "the latest fornax"
	if tag != "" {
		release = "fornax " + tag
	}
	fmt.Fprintf(os.Stderr, "fornax isn't installed on %s. Install %s there into ~/.local/bin? [Y/n] ", ui.Bold(host), release)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && answer == "" {
		fmt.Fprintln(os.Stderr)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// Runs install.sh on host, which checks the binary against the release's
// sha256sums.txt. The script goes over stdin to a bare `sh -s`: some sshds
// re-quote the command line on the way in (Windows OpenSSH with WSL's
// bash.exe as DefaultShell expands $vars early), and stdin reaches sh as is.
func installRemote(ctx context.Context, sshPath, host, tag string) error {
	cmd := exec.CommandContext(ctx, sshPath, "--", host, "sh -s")
	cmd.Stdin = strings.NewReader(remoteInstallScript(tag))
	remote := newRemoteLog(host, os.Stderr)
	cmd.Stdout = remote
	cmd.Stderr = remote
	err := cmd.Run()
	remote.flush()
	if err != nil {
		if last := lastLine(remote.last()); last != "" {
			return fmt.Errorf("couldn't install fornax on %s: %s", host, last)
		}
		return fmt.Errorf("couldn't install fornax on %s: %w", host, err)
	}
	return nil
}

// Saved before it runs, so a failed download fails the install instead of
// piping nothing into sh.
func remoteInstallScript(tag string) string {
	env := ""
	if tag != "" {
		env = "FORNAX_TAG=" + shQuote(tag) + " "
	}
	return fmt.Sprintf(`command -v curl >/dev/null 2>&1 || { echo "installing fornax needs curl" >&2; exit 1; }
t=$(mktemp) || exit 1
curl -fsSL %s -o "$t" && %ssh "$t" </dev/null
s=$?
rm -f "$t"
exit $s
`, shQuote(installScriptURL), env)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func lastLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// The remote's stderr (and ssh's own), passed through line by line under
// the host's name, keeping the tail to explain a failure with.
type remoteLog struct {
	host string
	out  io.Writer

	mu      sync.Mutex
	partial []byte
	tail    []string
}

const remoteLogTail = 12

func newRemoteLog(host string, out io.Writer) *remoteLog {
	return &remoteLog{host: host, out: out}
}

func (l *remoteLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.emitLocked(string(l.partial[:i]))
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

func (l *remoteLog) line(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emitLocked(s)
}

func (l *remoteLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.emitLocked(string(l.partial))
		l.partial = nil
	}
}

func (l *remoteLog) emitLocked(s string) {
	s = strings.TrimRight(s, "\r")
	fmt.Fprintf(l.out, "%s %s\n", ui.Dim(l.host+" │"), s)
	l.tail = append(l.tail, s)
	if len(l.tail) > remoteLogTail {
		l.tail = l.tail[len(l.tail)-remoteLogTail:]
	}
}

func (l *remoteLog) last() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.tail...)
}

// A context that ends when r does. sshd closes a channel's stdin when the
// connection drops, so a studio started with -leash dies with its tunnel.
func leashed(ctx context.Context, r io.Reader) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		io.Copy(io.Discard, r)
		cancel()
	}()
	return ctx
}
