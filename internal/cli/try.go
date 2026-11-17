package cli

// 'filter try' and 'router try' print the whole prompt the model was sent, so
// long blocks are written as indented prose below the table rows, not as
// cells.

import (
	"context"
	"fmt"
	"strings"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// tryAlias sends the prompts after the alias, each read with textOrFile.
func tryAlias[T any](ctx context.Context, a *aliasRun, path string, show func(*table, T)) (T, error) {
	var res T
	var prompts []string
	for _, arg := range a.fs.Args()[1:] {
		prompt, err := textOrFile(arg)
		if err != nil {
			return res, err
		}
		prompts = append(prompts, prompt)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body := control.TryRequest{Prompts: prompts}
	if err := a.c.do(ctx, "POST", path, body, &res); err != nil {
		return res, err
	}
	return res, out(a.asJSON, res, func(w *table) { show(w, res) })
}

func printFilterTry(w *table, t gateway.FilterTry) {
	show(w, "filter", t.Alias+" ("+string(t.Mode)+")")
	showIfSet(w, "model", t.Model)
	if t.Shadow {
		show(w, "enforcing", "no - this filter is in shadow, so nothing below is acted on")
	}
	printTryCost(w, t.CostMicros, t.Free)
	if t.Error != "" {
		show(w, "error", style.bad(t.Error))
	}
	var shown string
	for i, tr := range t.Trials {
		printTrialHead(w, i, tr.Prompt)
		show(w, "  outcome", filterOutcome(tr))
		if tr.After != "" {
			printText(w, "  forwarded as", tr.After)
		}
		if tr.Confidence > 0 {
			show(w, "  confidence", sureAndHow(tr.Confidence, tr.Read))
		} else if tr.Read == gateway.ReadText {
			show(w, "  read from", "the written word")
		}
		for _, hit := range tr.Hits {
			show(w, "  rule", hit.Rule+" ("+hitLabel(hit.Matches, hit.Refused)+")")
		}
		printWhy(w, tr.Explanation, tr.ExplainError)
		shown = printCalls(w, tr.Calls, shown)
	}
}

func filterOutcome(tr gateway.FilterTrial) string {
	switch tr.Outcome {
	case store.FilterRefuse:
		reason := tr.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return style.bad("refused") + ": " + reason
	case store.FilterRewrite:
		return style.warn("rewritten")
	case store.FilterError:
		return style.bad("could not run") + ": " + tr.Error
	}
	return style.ok("let through unchanged")
}

func hitLabel(matches int, refused bool) string {
	what := "matched once"
	if matches != 1 {
		what = fmt.Sprintf("matched %d times", matches)
	}
	if refused {
		what += ", and refused the request"
	}
	return what
}

func printRouterTry(w *table, t control.RouterTry) {
	show(w, "router", t.Router)
	for _, rt := range t.Routes {
		_, _ = fmt.Fprintln(w)
		printRouteTry(w, rt)
	}
}

func printRouteTry(w *table, t gateway.RouteTry) {
	show(w, "route step", t.Step+" ("+string(t.Mode)+")")
	showIfSet(w, "decides with", t.Model)
	if len(t.Destinations) > 0 {
		show(w, "offered", strings.Join(t.Destinations, ", "))
	}
	for _, d := range t.Dropped {
		show(w, "not offered", d)
	}
	printTryCost(w, t.CostMicros, false)
	if t.Error != "" {
		show(w, "error", style.bad(t.Error))
	}
	var shown string
	for i, tr := range t.Trials {
		printTrialHead(w, i, tr.Prompt)
		switch {
		case tr.Error != "":
			show(w, "  could not decide", style.bad(tr.Error))
			show(w, "  so it is", tr.Becomes)
		case tr.Confidence > 0:
			show(w, "  chose", style.ok(tr.Chosen)+", "+sureAndHow(tr.Confidence, tr.Read))
		default:
			show(w, "  chose", style.ok(tr.Chosen)+", read from what it wrote")
		}
		// The runners-up say how close a call it was.
		if len(tr.Shares) == len(t.Destinations) {
			for j, alias := range t.Destinations {
				show(w, "  "+alias, fmt.Sprintf("%.0f%%", tr.Shares[j]*100))
			}
		}
		printWhy(w, tr.Explanation, tr.ExplainError)
		shown = printCalls(w, tr.Calls, shown)
	}
}

// printTrialHead starts one prompt's part, named by its first line.
func printTrialHead(w *table, i int, prompt string) {
	_, _ = fmt.Fprintf(w, "\n%s\t%s\n", style.head(fmt.Sprintf("prompt %d", i+1)), firstLine(prompt))
}

func printTryCost(w *table, micros int64, free bool) {
	switch {
	case free:
		show(w, "cost", "nothing - no model runs")
	case micros > 0:
		show(w, "cost", policy.FormatMoney(micros))
	}
}

func sureAndHow(confidence float64, read string) string {
	how := "read from the written word"
	if read == gateway.ReadProbabilities {
		how = "read from the first token's probabilities"
	}
	return fmt.Sprintf("%.0f%% sure, %s", confidence*100, how)
}

func printWhy(w *table, explanation, failed string) {
	switch {
	case explanation != "":
		printText(w, "  why (asked afterwards)", explanation)
	case failed != "":
		show(w, "  why", style.muted("could not ask: "+failed))
	}
}

// printCalls prints each call: what was sent, what the model thought, and
// what it answered. A system message the same as the last one printed is
// named, not repeated: it is the same instruction for every prompt. It
// returns the last system message printed.
func printCalls(w *table, calls []gateway.ModelCall, shown string) string {
	for _, c := range calls {
		what := map[string]string{
			gateway.PurposeDecide:  "asked",
			gateway.PurposeRetry:   "asked again, another way",
			gateway.PurposeExplain: "asked why",
		}[c.Purpose]
		head := fmt.Sprintf("  %s %s at %s", what, c.Model, c.Path)
		switch {
		case !c.Sent:
			head += ", " + style.bad("not sent")
		case c.Status != 0:
			head += fmt.Sprintf(", %d in %dms", c.Status, c.MS)
		}
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, style.head(head))
		if len(c.Messages) == 0 {
			// A decision model's request is all fields, no messages.
			printText(w, "    request", string(c.Request))
		}
		for _, m := range c.Messages {
			if m.Role == "system" && m.Content == shown {
				_, _ = fmt.Fprintln(w, style.muted("    system: the same as above"))
				continue
			}
			if m.Role == "system" {
				shown = m.Content
			}
			printText(w, "    "+m.Role, m.Content)
		}
		if c.Reasoning != "" {
			printText(w, "    it thought", c.Reasoning)
		}
		switch {
		case c.Answer != "":
			printText(w, "    it answered", c.Answer)
		case c.Response != "":
			printText(w, "    it answered", c.Response)
		}
		if c.Error != "" {
			_, _ = fmt.Fprintln(w, "    "+style.bad(c.Error))
		}
	}
	return shown
}

// printText writes a label and then the text indented under it. Tabs would
// be read as table cells, so they become spaces.
func printText(w *table, label, text string) {
	_, _ = fmt.Fprintln(w, style.muted(label+":"))
	pad := strings.Repeat(" ", len(label)-len(strings.TrimLeft(label, " "))+2)
	_, _ = fmt.Fprintln(w, indent(strings.ReplaceAll(strings.TrimRight(text, "\n"), "\t", "    "), pad))
}
