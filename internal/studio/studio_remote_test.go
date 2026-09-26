package studio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSSHStudioArgs(t *testing.T) {
	args := sshStudioArgs("me@gpu-box", "fornax", 7360)
	want := []string{
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-L", "127.0.0.1:7360:127.0.0.1:7360",
		"--", "me@gpu-box",
	}
	if !slices.Equal(args[:len(want)], want) {
		t.Fatalf("ssh args:\n got %q\nwant %q", args[:len(want)], want)
	}
	if len(args) != len(want)+1 {
		t.Fatalf("want exactly one remote command after the host, got %q", args[len(want):])
	}
	remote := args[len(want)]
	if !strings.HasPrefix(remote, "sh -c '") {
		t.Errorf("remote command should run under an explicit sh: %q", remote)
	}
	for _, part := range []string{"$HOME/.local/bin", "$HOME/go/bin", "exec fornax studio -no-open -port 7360 -leash"} {
		if !strings.Contains(remote, part) {
			t.Errorf("remote command %q lacks %q", remote, part)
		}
	}
}

func TestRemoteStudioCommandQuoting(t *testing.T) {
	cases := map[string]string{
		"fornax":               "exec fornax studio",
		"/opt/fornax/fornax":   "exec /opt/fornax/fornax studio",
		"~/bin/fornax":         `exec "$HOME"/bin/fornax studio`,
		"/my tools/fornax":     `exec '\''/my tools/fornax'\'' studio`,
		"/x/it's; rm -rf ~/ #": `exec '\''/x/it'\''\'\'''\''s; rm -rf ~/ #'\'' studio`,
	}
	for path, want := range cases {
		if got := remoteStudioCommand(path, 7361); !strings.Contains(got, want) {
			t.Errorf("fornax path %q:\n got %s\nwant it to contain %s", path, got, want)
		}
	}
}

// The command has to survive two shells: the login shell sshd uses, then
// the sh it starts. Run it through two real ones and check the argv that
// comes out the far end.
func TestRemoteStudioCommandSurvivesTwoShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, path := range []string{"fornax", "/my tools/fornax", "/x/it's; echo pwned"} {
		cmd := strings.Replace(remoteStudioCommand(path, 7362), "exec ", `printf "%s\n" `, 1)
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		got := strings.Split(strings.TrimSpace(string(out)), "\n")
		want := []string{path, "studio", "-no-open", "-port", "7362", "-leash"}
		if !slices.Equal(got, want) {
			t.Errorf("%q came out as %q", path, got)
		}
	}
}

func TestReadStudioLink(t *testing.T) {
	const link = "http://127.0.0.1:7360/?key=esk_local_abc"
	var noise []string
	r := bufio.NewReader(strings.NewReader("Welcome to gpu-box\r\nhttp://127.0.0.1:9999/?key=wrong\n" + link + "\nafter\n"))
	got, err := readStudioLink(r, 7360, time.Second, func(s string) { noise = append(noise, s) })
	if err != nil || got != link {
		t.Fatalf("got %q, %v; want %q", got, err, link)
	}
	if len(noise) != 2 || noise[0] != "Welcome to gpu-box" {
		t.Errorf("lines before the link should go to other, got %q", noise)
	}

	if _, err := readStudioLink(bufio.NewReader(strings.NewReader("bye\n")), 7360, time.Second, nil); !errors.Is(err, errNoStudioLink) {
		t.Errorf("EOF before a link: got %v, want errNoStudioLink", err)
	}
	if _, err := readStudioLink(bufio.NewReader(strings.NewReader("http://127.0.0.1:7360/?key=")), 7360, time.Second, nil); !errors.Is(err, errNoStudioLink) {
		t.Errorf("a link without a key is no link: got %v", err)
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	start := time.Now()
	if _, err := readStudioLink(bufio.NewReader(pr), 7360, 50*time.Millisecond, nil); !errors.Is(err, errStudioLinkLate) {
		t.Errorf("silent remote: got %v, want errStudioLinkLate", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the timeout did not bound the wait")
	}
}

func TestTunnelFailure(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		lines []string
		want  string
		busy  bool
	}{
		{"remote port taken", studioLeashBusy, []string{"port 7360 is in use by something else"}, "", true},
		{"local forward taken", 255, []string{"bind [127.0.0.1]:7360: Address already in use", "channel_setup_fwd_listener_tcpip: cannot listen to port: 7360", "Could not request local forwarding."}, "", true},
		{"auth", 255, []string{"me@gpu-box: Permission denied (publickey)."}, "Permission denied", false},
		{"unreachable", 255, []string{"ssh: connect to host gpu-box port 22: Connection refused"}, "couldn't reach gpu-box over ssh: ssh: connect to host gpu-box port 22: Connection refused", false},
		{"no fornax", 127, []string{"sh: 1: exec: fornax: not found"}, "install.sh", false},
		{"old fornax", 2, []string{"flag provided but not defined: -leash"}, "fornax upgrade", false},
		{"windows", 1, []string{"'sh' is not recognized as an internal or external command,"}, "Linux or macOS", false},
		{"early exit", 1, []string{"fornax: permission denied: /root/.fornax"}, "exit 1): fornax: permission denied", false},
	}
	for _, c := range cases {
		err := tunnelFailure("gpu-box", c.code, c.lines)
		if c.busy {
			if !errors.Is(err, errTunnelPortBusy) {
				t.Errorf("%s: got %v, want a retry on the next port", c.name, err)
			}
			continue
		}
		if err == nil || errors.Is(err, errTunnelPortBusy) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestRemoteLogPrefixesAndKeepsTail(t *testing.T) {
	var out bytes.Buffer
	l := newRemoteLog("gpu-box", &out)
	io.WriteString(l, "one\ntw")
	io.WriteString(l, "o\r\n")
	for i := range remoteLogTail + 3 {
		io.WriteString(l, strings.Repeat("x", i)+"\n")
	}
	io.WriteString(l, "partial")
	l.flush()
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(out.String(), "")
	if !strings.Contains(plain, "gpu-box │ one\n") || !strings.Contains(plain, "gpu-box │ two\n") {
		t.Errorf("lines should pass through under the host's name:\n%s", plain)
	}
	tail := l.last()
	if len(tail) != remoteLogTail || tail[len(tail)-1] != "partial" {
		t.Errorf("tail: got %d lines ending %q", len(tail), tail[len(tail)-1])
	}
}

func TestLeashEndsWithStdin(t *testing.T) {
	pr, pw := io.Pipe()
	ctx := leashed(context.Background(), pr)
	select {
	case <-ctx.Done():
		t.Fatal("leash ended while stdin was open")
	case <-time.After(50 * time.Millisecond):
	}
	pw.Close()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("leash outlived its stdin")
	}

	parent, cancel := context.WithCancel(context.Background())
	pr2, pw2 := io.Pipe()
	defer pw2.Close()
	ctx = leashed(parent, pr2)
	cancel()
	if ctx.Err() == nil {
		t.Error("a leashed context should still end with its parent")
	}
}

func TestStudioAbout(t *testing.T) {
	s := testStudio(t)
	s.remote = true
	rec := studioGet(t, s.handler(7340), "GET", "/api/about", "127.0.0.1:7340", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("about: %d", rec.Code)
	}
	var about map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &about); err != nil {
		t.Fatal(err)
	}
	if about["os"] != runtime.GOOS || about["arch"] != runtime.GOARCH || about["remote"] != true {
		t.Errorf("about: %v", about)
	}
	if _, ok := about["backend"]; !ok {
		t.Error("about should carry a backend field, even empty")
	}
	if rec := studioGet(t, s.handler(7340), "GET", "/api/about", "127.0.0.1:7340", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("about without a key: got %d, want 401", rec.Code)
	}
}

func TestStudioRemotePortsStayInTheirRange(t *testing.T) {
	if RemotePortBase <= Port || RemotePortBase < 7355 || studioRemotePortEnd > 7401 {
		t.Errorf("tunnel ports %d–%d overlap the studio, built-in or custom model ports", RemotePortBase, studioRemotePortEnd-1)
	}
}
