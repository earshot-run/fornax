package main

// `judge` and the /v1/systemone protocol — typed questions over a piece of
// state, answered with calibrated probabilities. kev and laya both serve
// this shape; the only difference is the model name in the request and
// that laya requires the loopback key.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/ui"
)

// A typed question in the request: noul (yes/no), choice (one of a set) or
// score (a level on an ordered scale). The wire shape lives in openai, which
// both `judge` and the MCP server speak.
type typedQuestion = openai.TypedQuestion

// Print the answers as probability bars: one headline per question, then the
// option distribution underneath when there is one.
func printJudgeAnswers(reply map[string]any, order []string) {
	answers, _ := reply["answers"].(map[string]any)
	if answers == nil {
		data, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Println(string(data))
		return
	}
	ids := order
	if len(ids) == 0 {
		for id := range answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	idWidth := 0
	for _, id := range ids {
		if len(id) > idWidth {
			idWidth = len(id)
		}
	}
	for _, id := range ids {
		a, ok := answers[id].(map[string]any)
		if !ok {
			continue
		}
		name := ui.Cell(id, idWidth, ui.Bold)
		switch a["type"] {
		case "noul":
			if v, ok := a["noul"].(float64); ok {
				fmt.Printf("  %s %s %.2f  %s\n", name, ui.Cell("noul", 7, ui.Dim), v, ui.Bar(v, ui.Cyan))
			}
		case "choice":
			fmt.Printf("  %s %s %s\n", name, ui.Cell("choice", 7, ui.Dim), ui.Green(fmt.Sprint(a["choice"])))
			printJudgeProbs(a)
		case "score":
			if v, ok := a["score"].(float64); ok {
				fmt.Printf("  %s %s %.2f → %s\n", name, ui.Cell("score", 7, ui.Dim), v, ui.Cyan(scoreLabel(a, v)))
				printJudgeProbs(a)
			}
		}
	}
	if ms, ok := reply["latency_ms"].(float64); ok {
		fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("%.0f ms", ms)))
	}
}

// For a score answer the winning label comes from the legend ("1" → "frustrated").
func scoreLabel(answer map[string]any, v float64) string {
	legend, _ := answer["legend"].(map[string]any)
	key := fmt.Sprint(int(v + 0.5))
	if s, ok := legend[key].(string); ok {
		return s
	}
	return fmt.Sprintf("%.0f", v)
}

// The option distribution, best first, as small bars.
func printJudgeProbs(answer map[string]any) {
	probs, _ := answer["probabilities"].(map[string]any)
	legend, _ := answer["legend"].(map[string]any)
	type row struct {
		label string
		p     float64
	}
	var rows []row
	for k, raw := range probs {
		p, ok := raw.(float64)
		if !ok {
			continue
		}
		label := k
		if s, ok := legend[k].(string); ok {
			label = s
		}
		rows = append(rows, row{label, p})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].p > rows[j].p })
	width := 0
	for _, r := range rows {
		if len(r.label) > width {
			width = len(r.label)
		}
	}
	for _, r := range rows {
		fmt.Printf("      %s %.2f  %s\n", ui.Cell(r.label, width, nil), r.p, ui.Bar(r.p, ui.Cyan))
	}
}

// A canned request for `test` and `bench`: three question shapes at once.
func judgeProbeQuestions() (map[string]*typedQuestion, []string) {
	return map[string]*typedQuestion{
		"escalate": {Kind: "noul", Instructions: "Does this need urgent human attention?"},
		"team": {Kind: "choice", Instructions: "Which team should handle this?",
			Options: []string{"returns", "shipping", "billing"}},
		"mood": {Kind: "score", Instructions: "How frustrated is the customer?",
			Options: []string{"calm", "frustrated", "angry"}},
	}, []string{"escalate", "team", "mood"}
}

const judgeProbeState = "Shoes arrived two weeks late and in the wrong size. Also I see two charges on my card."

func runDecisionTest(ctx context.Context, spec *catalog.Spec) error {
	return modelrt.WithServer(ctx, spec, nil, func(url, key string) error {
		qs, order := judgeProbeQuestions()
		reply, err := openai.SystemOne(ctx, url, key, modelrt.DecisionModel(spec), judgeProbeState, qs)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", ui.Green("✓"), ui.Bold(spec.ID))
		printJudgeAnswers(reply, order)
		return nil
	})
}

// Median latency over a handful of identical requests — the prefix cache makes
// later ones representative of steady state.
func runDecisionBench(ctx context.Context, spec *catalog.Spec, runs int) error {
	return modelrt.WithServer(ctx, spec, nil, func(url, key string) error {
		qs, _ := judgeProbeQuestions()
		var lat []float64
		for i := 0; i < runs; i++ {
			reply, err := openai.SystemOne(ctx, url, key, modelrt.DecisionModel(spec), judgeProbeState, qs)
			if err != nil {
				return err
			}
			if ms, ok := reply["latency_ms"].(float64); ok {
				lat = append(lat, ms)
				fmt.Printf("  %s %.0f ms\n", ui.Dim(fmt.Sprintf("run %d", i+1)), ms)
			}
		}
		if len(lat) == 0 {
			return fmt.Errorf("the server reported no latencies")
		}
		sort.Float64s(lat)
		fmt.Printf("%s %s — median %.0f ms over %d requests\n", ui.Green("✓"), ui.Bold(spec.ID), lat[len(lat)/2], len(lat))
		return nil
	})
}

// `judge` — ask a live decision model typed questions about a piece of state.
// --ask 'id|type|instructions|opt1|opt2|…'  (repeatable; noul takes no options)
// or --json <file|-> for a raw SystemOne request body. Input is validated and
// read before any server work so a bad flag fails fast.
func runJudge(ctx context.Context, spec *catalog.Spec, state string, asks []string, jsonPath string) error {
	var raw []byte
	var questions map[string]*typedQuestion
	var order []string
	if jsonPath != "" {
		var err error
		if jsonPath == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(jsonPath)
		}
		if err != nil {
			return err
		}
	} else {
		questions = map[string]*typedQuestion{}
		for _, ask := range asks {
			parts := strings.Split(ask, "|")
			if len(parts) < 3 {
				return fmt.Errorf("--ask %q needs id|type|instructions[|options…]", ask)
			}
			q := &typedQuestion{Kind: parts[1], Instructions: parts[2], Options: parts[3:]}
			switch q.Kind {
			case "noul":
				if len(q.Options) > 0 {
					return fmt.Errorf("--ask %q: noul takes no options", ask)
				}
			case "choice", "score":
				if len(q.Options) < 2 {
					return fmt.Errorf("--ask %q: %s needs at least two options", ask, q.Kind)
				}
			default:
				return fmt.Errorf("--ask %q: type must be noul, choice or score", ask)
			}
			questions[parts[0]] = q
			order = append(order, parts[0])
		}
		if len(questions) == 0 {
			return fmt.Errorf("no questions — pass --ask 'id|noul|instructions' (or --json)")
		}
	}
	return modelrt.WithServer(ctx, spec, nil, func(url, key string) error {
		if jsonPath != "" {
			resp, err := openai.Post(ctx, url+"/systemone", key, raw)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return openai.Error(resp)
			}
			var parsed map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				return err
			}
			printJudgeAnswers(parsed, nil)
			return nil
		}
		reply, err := openai.SystemOne(ctx, url, key, modelrt.DecisionModel(spec), state, questions)
		if err != nil {
			return err
		}
		printJudgeAnswers(reply, order)
		return nil
	})
}

// Repeatable --ask flag for `judge`.
type askFlags []string

func (f *askFlags) String() string { return "" }
func (f *askFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func cmdJudge(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("judge", flag.ExitOnError)
	state := set.String("state", "", "the document the questions judge")
	jsonPath := set.String("json", "", "a raw SystemOne request file ('-' reads stdin)")
	var asks askFlags
	set.Var(&asks, "ask", "typed question: 'id|noul|instructions' or 'id|choice|instructions|opt1|opt2…' (repeatable)")
	set.Usage = ui.UsageFunc(set, `usage: fornax judge <model> --state "text" --ask 'id|type|instructions[|opts…]' [--ask …]
       fornax judge <model> --json request.json   ('-' reads stdin)

examples:
  fornax judge kev-4b --state "my order never arrived and I was charged twice" \
    --ask 'escalate|noul|Needs urgent human attention?' \
    --ask 'team|choice|Which team?|returns|shipping|billing' \
    --ask 'mood|score|How upset?|calm|frustrated|angry'`)
	got := parseFlexible(set, args, 1)
	if len(got) == 0 {
		return fmt.Errorf("usage: fornax judge <model> [--state …] [--ask …|--json …]")
	}
	id := got[0]
	spec, _, err := modelrt.Resolve(ctx, id)
	if err != nil {
		return err
	}
	if spec.Kind != catalog.Decision {
		return fmt.Errorf("%s chats, it does not judge — `judge` is for decision models (kev, laya)", spec.ID)
	}
	if *jsonPath != "" && (*state != "" || len(asks) > 0) {
		return fmt.Errorf("--json is a complete request body — drop --state and --ask")
	}
	if *jsonPath == "" && *state == "" {
		return fmt.Errorf("nothing to judge — pass --state \"text\" or --json request.json")
	}
	return runJudge(ctx, spec, *state, asks, *jsonPath)
}
