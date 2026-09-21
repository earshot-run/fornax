package main

// Download progress. On a terminal: one animated line with a bar, rate and
// ETA. Off a terminal: a handful of plain milestone lines so logs stay clean.

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type progressBar struct {
	label   string
	total   int64
	started time.Time
	last    time.Time
	next    int64 // next non-tty milestone
}

func newProgress(label string, total int64) *progressBar {
	return &progressBar{label: label, total: total, started: time.Now(), next: total / 4}
}

func (p *progressBar) set(done int64) {
	now := time.Now()
	if events != nil {
		if done >= p.total || now.Sub(p.last) >= progressEvery {
			p.last = now
			emit("progress", map[string]any{"label": p.label, "done": done, "total": p.total})
		}
		return
	}
	elapsed := now.Sub(p.started).Seconds()
	if done >= p.total && elapsed < 1 {
		fmt.Fprintf(os.Stderr, "\r\x1b[K%s %6s — done\n", p.label, humanSize(done))
		return
	}
	if !ansiOn {
		const marks = 4 // report at 25% steps when there is no line to redraw
		if done >= p.total || done >= p.next {
			p.next = done + p.total/marks
			fmt.Fprintf(os.Stderr, "%s %d%% (%s of %s)\n",
				p.label, done*100/max64(p.total, 1), humanSize(done), humanSize(p.total))
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
	bar := strings.Repeat("█", int(fill)) + dim(strings.Repeat("░", width-int(fill)))
	var rate float64
	if elapsed > 0 {
		rate = float64(done) / elapsed
	}
	eta := dim("  ?")
	if rate > 0 && done < p.total {
		eta = fmt.Sprintf("%4ds", int(float64(p.total-done)/rate))
	}
	fmt.Fprintf(os.Stderr, "\r\x1b[K%s %s %3d%% %s/%s %s/s %s",
		bold(p.label), bar, done*100/max64(p.total, 1),
		humanSize(done), humanSize(p.total), humanSize(int64(rate)), eta)
	if done >= p.total {
		fmt.Fprintln(os.Stderr)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
