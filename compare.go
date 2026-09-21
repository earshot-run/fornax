package main

// `fornax compare a,b,c "prompt"` — the same prompt to several models, one
// at a time. Parallel loads would split the RAM each model gets, so they
// run sequentially and each reply prints as it lands, slowest last in the
// final ranking.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type compareResult struct {
	id        string
	text      string
	elapsed   time.Duration
	tokPerSec float64 // 0 when the server reports no usage
}

func cmdCompare(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: fornax compare <model,model,…> <prompt…>")
	}
	prompt := strings.TrimSpace(strings.Join(args[1:], " "))
	if prompt == "" {
		return fmt.Errorf("usage: fornax compare <model,model,…> <prompt…>")
	}
	var specs []*modelSpec
	var engs []*engineSpec
	seen := map[string]bool{}
	for _, raw := range strings.Split(args[0], ",") {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		spec, eng, err := resolve(id)
		if err != nil {
			return err
		}
		if spec.rt == runtimeKev {
			return fmt.Errorf("%s judges, it doesn't chat — drop it or use `fornax judge %s`", spec.id, spec.id)
		}
		if spec.rt == runtimeSD {
			return fmt.Errorf("%s draws, it doesn't chat — drop it or use `fornax draw %s`", spec.id, spec.id)
		}
		if spec.kind == modalEmbed {
			return fmt.Errorf("%s embeds, it doesn't chat — drop it or use `fornax embed %s`", spec.id, spec.id)
		}
		specs = append(specs, spec)
		engs = append(engs, eng)
	}
	if len(specs) < 2 {
		return fmt.Errorf("compare needs at least two models — fornax compare <model,model,…> <prompt…>")
	}

	var results []compareResult
	failures := 0
	for i, spec := range specs {
		res := compareResult{id: spec.id}
		err := withServer(ctx, spec, engs[i], func(url, key string) error {
			started := time.Now()
			reply, err := chatOnce(ctx, url, key, spec.id,
				[]message{textMessage("user", prompt)}, -1)
			res.elapsed = time.Since(started)
			if err != nil {
				return err
			}
			if strings.TrimSpace(reply.Text) == "" {
				return fmt.Errorf("the model returned an empty reply")
			}
			res.text = strings.TrimSpace(reply.Text)
			if reply.Usage != nil {
				res.tokPerSec = float64(reply.Usage.CompletionTokens) / res.elapsed.Seconds()
			}
			return nil
		})
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("─── %s ───  failed: %v", spec.id, err)))
			continue
		}
		results = append(results, res)
		fmt.Println(dim(compareHeader(res)))
		fmt.Println(res.text)
		fmt.Println()
	}
	if len(results) == 0 {
		return fmt.Errorf("compare failed for all %d models", len(specs))
	}

	sort.Slice(results, func(i, j int) bool { return results[i].elapsed < results[j].elapsed })
	ranked := make([]string, len(results))
	for i, r := range results {
		ranked[i] = fmt.Sprintf("%s (%s)", r.id, compareStats(r))
	}
	fmt.Println(dim("fastest first: " + strings.Join(ranked, " · ")))
	return nil
}

func compareHeader(r compareResult) string {
	return fmt.Sprintf("─── %s ───  (%s)", r.id, compareStats(r))
}

func compareStats(r compareResult) string {
	s := fmt.Sprintf("%.1fs", r.elapsed.Seconds())
	if r.tokPerSec > 0 {
		if r.tokPerSec < 10 {
			s += fmt.Sprintf(" · ~%.1f tok/s", r.tokPerSec)
		} else {
			s += fmt.Sprintf(" · ~%.0f tok/s", r.tokPerSec)
		}
	}
	return s
}
