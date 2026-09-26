package ui

// Download progress. On a terminal: one animated line with a bar, rate and
// ETA. Off a terminal: a handful of plain milestone lines so logs stay clean.
// With --events on, neither: one JSON progress line for the supervisor.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/events"
)

type Progress struct {
	label   string
	total   int64
	started time.Time
	last    time.Time
	next    int64 // next non-tty milestone
}

func NewProgress(label string, total int64) *Progress {
	return &Progress{label: label, total: total, started: time.Now(), next: total / 4}
}

func (p *Progress) Set(done int64) {
	now := time.Now()
	if events.On() {
		if done >= p.total || now.Sub(p.last) >= events.ProgressEvery {
			p.last = now
			events.Emit("progress", map[string]any{"label": p.label, "done": done, "total": p.total})
		}
		return
	}
	elapsed := now.Sub(p.started).Seconds()
	if done >= p.total && elapsed < 1 {
		fmt.Fprintf(os.Stderr, "\r\x1b[K%s %6s — done\n", p.label, HumanSize(done))
		return
	}
	if !ansiOn {
		const marks = 4 // report at 25% steps when there is no line to redraw
		if done >= p.total || done >= p.next {
			p.next = done + p.total/marks
			fmt.Fprintf(os.Stderr, "%s %d%% (%s of %s)\n",
				p.label, done*100/max(p.total, 1), HumanSize(done), HumanSize(p.total))
		}
		return
	}
	if now.Sub(p.last) < 100*time.Millisecond && done < p.total {
		return
	}
	p.last = now
	const width = 24
	fill := int64(0)
	if p.total > 0 {
		fill = int64(width) * done / p.total
	}
	rendered := strings.Repeat("█", int(fill)) + Dim(strings.Repeat("░", width-int(fill)))
	var rate float64
	if elapsed > 0 {
		rate = float64(done) / elapsed
	}
	eta := Dim("  ?")
	if rate > 0 && done < p.total {
		eta = fmt.Sprintf("%4ds", int(float64(p.total-done)/rate))
	}
	fmt.Fprintf(os.Stderr, "\r\x1b[K%s %s %3d%% %s/%s %s/s %s",
		Bold(p.label), rendered, done*100/max(p.total, 1),
		HumanSize(done), HumanSize(p.total), HumanSize(int64(rate)), eta)
	if done >= p.total {
		fmt.Fprintln(os.Stderr)
	}
}
