package main

// `record` — mic to WAV with the OS's own recorder, no engine and no
// models: afrecord on macOS, arecord (or ffmpeg on ALSA) on Linux. The take
// it writes is what `hear` and `talk` consume.

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// `fornax record <out.wav> [-d seconds] [-r rate]`
func cmdRecord(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("record", flag.ExitOnError)
	dur := set.Float64("d", 0, "stop after this many seconds (default: until ctrl-c)")
	rate := set.Int("r", 16000, "sample rate in Hz")
	usageLine := `usage: fornax record <out.wav> [-d seconds] [-r rate]`
	set.Usage = func() { fmt.Fprintln(os.Stderr, usageLine) }
	var outPath string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		outPath, args = args[0], args[1:]
	}
	set.Parse(args)
	rest := set.Args()
	if outPath == "" && len(rest) > 0 {
		outPath, rest = rest[0], rest[1:]
	}
	if outPath == "" || len(rest) > 0 {
		return fmt.Errorf("%s", usageLine)
	}
	if !strings.HasSuffix(strings.ToLower(outPath), ".wav") {
		outPath += ".wav"
	}
	if *dur < 0 {
		return fmt.Errorf("-d must be positive seconds, got %g", *dur)
	}
	if *rate <= 0 {
		return fmt.Errorf("-r wants a positive sample rate, got %d", *rate)
	}
	tool, recArgs, err := recordArgs(outPath, *dur, *rate)
	if err != nil {
		return err
	}
	if *dur <= 0 && isTTY(os.Stdin) {
		fmt.Fprintf(os.Stderr, "%s\n", dim("recording… ctrl-c to stop"))
	}
	cmd := exec.CommandContext(ctx, tool, recArgs...)
	// afrecord/arecord only finalize the WAV header on a clean stop, so
	// ctrl-c asks politely (SIGINT) instead of the default kill; WaitDelay
	// is the backstop that still reaps a wedged recorder.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)
	if runErr != nil && ctx.Err() == nil {
		return fmt.Errorf("%s exited: %w", filepath.Base(tool), runErr)
	}
	// Ctrl-c is the stop button, not a failure — report the take either way.
	info, err := os.Stat(outPath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("%s wrote no audio to %s", filepath.Base(tool), outPath)
	}
	fmt.Printf("%s wrote %s (%.1fs)\n", green("✓"), outPath, elapsed.Seconds())
	return nil
}

// The recorder this OS has, and its argv. dur 0 records until the signal.
func recordArgs(outPath string, dur float64, rate int) (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		if tool, err := exec.LookPath("afrecord"); err == nil {
			args := []string{"-f", "WAVE", "-d", "LEI16", "-r", strconv.Itoa(rate)}
			if dur > 0 {
				args = append(args, "-t", strconv.FormatFloat(dur, 'g', -1, 64))
			}
			return tool, append(args, outPath), nil
		}
		// afrecord is gone from current macOS; ffmpeg's avfoundation
		// input is the fallback. ":default" is the system default mic.
		if tool, err := exec.LookPath("ffmpeg"); err == nil {
			args := []string{"-y", "-loglevel", "error", "-f", "avfoundation", "-i", ":default", "-ar", strconv.Itoa(rate), "-ac", "1"}
			if dur > 0 {
				args = append(args, "-t", strconv.FormatFloat(dur, 'g', -1, 64))
			}
			return tool, append(args, outPath), nil
		}
		return "", nil, fmt.Errorf("record needs afrecord or ffmpeg — install ffmpeg (`brew install ffmpeg`)")
	case "linux":
		if tool, err := exec.LookPath("arecord"); err == nil {
			args := []string{"-f", "cd", "-t", "wav", "-r", strconv.Itoa(rate)}
			if dur > 0 {
				args = append(args, "-d", strconv.Itoa(int(math.Ceil(dur))))
			}
			return tool, append(args, outPath), nil
		}
		if tool, err := exec.LookPath("ffmpeg"); err == nil {
			args := []string{"-y", "-loglevel", "error", "-f", "alsa", "-i", "default", "-ar", strconv.Itoa(rate)}
			if dur > 0 {
				args = append(args, "-t", strconv.FormatFloat(dur, 'g', -1, 64))
			}
			return tool, append(args, outPath), nil
		}
		return "", nil, fmt.Errorf("record needs arecord or ffmpeg — install alsa-utils or ffmpeg")
	}
	return "", nil, fmt.Errorf("record needs afrecord (macOS) or arecord/ffmpeg (Linux)")
}
