package ui

// Package ui is fornax's terminal chrome: colors, a spinner for long waits,
// progress bars, and the tables `-h` prints. Everything degrades to plain
// text when stderr is not a terminal, NO_COLOR is set, or TERM is dumb —
// piped output never sees an escape code.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var ansiOn = detectANSI()

func detectANSI() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTTY(os.Stderr) && vtReady(os.Stderr)
}

func IsTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too; `2>/dev/null` is not a terminal.
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}

func style(s, code string) string {
	if !ansiOn || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string    { return style(s, "1") }
func Dim(s string) string     { return style(s, "2") }
func Red(s string) string     { return style(s, "31") }
func Green(s string) string   { return style(s, "32") }
func Yellow(s string) string  { return style(s, "33") }
func Cyan(s string) string    { return style(s, "36") }
func Magenta(s string) string { return style(s, "35") }
func Blue(s string) string    { return style(s, "34") }
func Pink(s string) string    { return style(s, "95") }

func MarkOK() string   { return Green("●") }
func MarkIdle() string { return Dim("○") }

// Bar renders a fraction as a 20-cell bar; the filled part is colored.
func Bar(frac float64, fill func(string) string) string {
	const width = 20
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac*width + 0.5)
	return fill(strings.Repeat("█", n)) + Dim(strings.Repeat("░", width-n))
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type Spinner struct {
	done chan struct{}
	wg   sync.WaitGroup
}

// Spin shows a waiting indicator on stderr. Without a terminal it prints
// one plain line instead of animating.
func Spin(label string) *Spinner {
	s := &Spinner{done: make(chan struct{})}
	if !ansiOn {
		fmt.Fprintf(os.Stderr, "%s…\n", label)
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-s.done:
				return
			case <-tick.C:
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s %s", Cyan(spinFrames[i%len(spinFrames)]), Dim(label+"…"))
			}
		}
	}()
	return s
}

// Stop clears the spinner line; when final is set it stays as the result.
func (s *Spinner) Stop(final string) {
	close(s.done)
	s.wg.Wait()
	if ansiOn {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
	if final != "" {
		fmt.Fprintln(os.Stderr, final)
	}
}

// PrintUsage writes help text with its section headers picked out in cyan.
func PrintUsage(w io.Writer, text string) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, " ") {
			fmt.Fprintln(w, Cyan(Bold(line)))
			continue
		}
		fmt.Fprintln(w, line)
	}
}

// A command's help: its usage line, then the flags it takes in the same
// two-column shape as the command table in `fornax -h`.
func UsageFunc(set *flag.FlagSet, lines string) func() {
	return func() {
		fmt.Fprintln(os.Stderr, lines)
		PrintFlags(set)
	}
}

func PrintFlags(set *flag.FlagSet) {
	type entry struct{ label, help string }
	var entries []entry
	width := 0
	set.VisitAll(func(f *flag.Flag) {
		kind, help := flag.UnquoteUsage(f)
		label := "-" + f.Name
		if kind != "" {
			label += " " + kind
		}
		if !strings.Contains(help, "default") && !zeroFlag(f.DefValue) {
			help += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		if len(label) > width {
			width = len(label)
		}
		entries = append(entries, entry{label, help})
	})
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", Cyan(Bold("flags:")))
	for _, item := range entries {
		fmt.Fprintf(os.Stderr, "  %s  %s\n", Cell(item.label, width, Bold), Dim(item.help))
	}
}

// A default worth printing is one the operator did not already get.
func zeroFlag(value string) bool {
	switch value {
	case "", "0", "false", "0s":
		return true
	}
	return false
}

// Cell pads to a visible width, then styles — %-Ns would count the escape
// codes.
func Cell(s string, width int, styleFn func(string) string) string {
	for len(s) < width {
		s += " "
	}
	if styleFn == nil {
		return s
	}
	return styleFn(s)
}

const gib = 1 << 30

// HumanSize is a byte count as the operator reads it.
func HumanSize(bytes int64) string {
	if bytes >= gib {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gib))
	}
	if bytes >= 1<<20 {
		return fmt.Sprintf("%.0f MB", float64(bytes)/float64(1<<20))
	}
	return fmt.Sprintf("%.0f KB", float64(bytes)/float64(1<<10))
}
