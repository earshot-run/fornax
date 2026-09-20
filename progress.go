package main

// A one-line download progress display: bytes, percent, rate, ETA.
// Writes to stderr so `fornax pull > file` stays clean.

import (
	"fmt"
	"os"
	"time"
)

type progressBar struct {
	label   string
	total   int64
	started time.Time
	last    time.Time
}

func newProgress(label string, total int64) *progressBar {
	return &progressBar{label: label, total: total, started: time.Now()}
}

func (p *progressBar) set(done int64) {
	now := time.Now()
	if now.Sub(p.last) < 200*time.Millisecond && done < p.total {
		return
	}
	p.last = now
	elapsed := now.Sub(p.started).Seconds()
	if done >= p.total && elapsed < 1 {
		fmt.Fprintf(os.Stderr, "%-12s %6s — done\n", p.label, humanSize(done))
		return
	}
	var rate float64
	if elapsed > 0 {
		rate = float64(done) / elapsed
	}
	var eta string
	if rate > 0 {
		eta = fmt.Sprintf("%ds", int(float64(p.total-done)/rate))
	} else {
		eta = "?"
	}
	fmt.Fprintf(os.Stderr, "\r%-12s %6s / %-9s %8s/s %5s ", p.label, humanSize(done), humanSize(p.total), humanSize(int64(rate)), eta)
	if done >= p.total {
		fmt.Fprintln(os.Stderr)
	}
}
