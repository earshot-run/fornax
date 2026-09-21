package main

// Terminal chrome: colors, a spinner for long waits, bars. Everything
// degrades to plain text when stderr is not a terminal, NO_COLOR is set,
// or TERM is dumb — piped output never sees an escape code.

import (
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
	return isTTY(os.Stderr) && vtReady(os.Stderr)
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func style(s, code string) string {
	if !ansiOn || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string    { return style(s, "1") }
func dim(s string) string     { return style(s, "2") }
func red(s string) string     { return style(s, "31") }
func green(s string) string   { return style(s, "32") }
func yellow(s string) string  { return style(s, "33") }
func cyan(s string) string    { return style(s, "36") }
func magenta(s string) string { return style(s, "35") }

func markOK() string   { return green("●") }
func markIdle() string { return dim("○") }

// A fraction rendered as a 20-cell bar; the filled part is colored.
func bar(frac float64, fill func(string) string) string {
	const width = 20
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac*width + 0.5)
	return fill(strings.Repeat("█", n)) + dim(strings.Repeat("░", width-n))
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinner struct {
	done chan struct{}
	wg   sync.WaitGroup
}

// Show a waiting indicator on stderr. Without a terminal it prints one
// plain line instead of animating.
func spin(label string) *spinner {
	s := &spinner{done: make(chan struct{})}
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
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s %s", cyan(spinFrames[i%len(spinFrames)]), dim(label+"…"))
			}
		}
	}()
	return s
}

// Clear the spinner line; when final is set it stays as the result.
func (s *spinner) stop(final string) {
	close(s.done)
	s.wg.Wait()
	if ansiOn {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
	if final != "" {
		fmt.Fprintln(os.Stderr, final)
	}
}

// Usage text with its section headers picked out in cyan.
func printUsage(w io.Writer) {
	for _, line := range strings.Split(usage, "\n") {
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, " ") {
			fmt.Fprintln(w, cyan(bold(line)))
			continue
		}
		fmt.Fprintln(w, line)
	}
}
