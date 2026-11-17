package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/store"
)

func call(system, user, answer string) gateway.ModelCall {
	return gateway.ModelCall{
		Purpose: "decide", Model: "keera-speed", Path: "/chat/completions", Sent: true,
		Status: 200, Answer: answer,
		Messages: []gateway.CallMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
	}
}

func TestATryPrintsTheWholePromptAndTheInstructionOnce(t *testing.T) {
	const system = "Refuse client data.\n--- how to answer ---\n\tAnswer ALLOW or REFUSED."
	try := gateway.FilterTry{Alias: "no-client-data", Mode: "gate", Model: "keera-speed", Trials: []gateway.FilterTrial{
		{
			Prompt: "Summarise the Meier account", Outcome: store.FilterRefuse, Reason: "names a client",
			Confidence: 0.97, Read: "probabilities",
			Asked: gateway.Asked{
				Explanation: "It names a client.",
				Calls: []gateway.ModelCall{
					call(system, `["Summarise the Meier account"]`, "REFUSED: names a client"),
				},
			},
		},
		{
			Prompt: "Rename tmp2", Outcome: store.FilterPass,
			Asked: gateway.Asked{Calls: []gateway.ModelCall{call(system, `["Rename tmp2"]`, "ALLOW")}},
		},
	}}

	var buf bytes.Buffer
	w := newTable(&buf)
	printFilterTry(w, try)
	_ = w.Flush()
	got := buf.String()

	for _, want := range []string{
		"refused: names a client",
		"97% sure, read from the first token's probabilities",
		"It names a client.",
		`["Summarise the Meier account"]`,
		"--- how to answer ---",
		"REFUSED: names a client",
		"let through unchanged",
		"system: the same as above",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "Refuse client data."); n != 1 {
		t.Errorf("the instruction was printed %d times, want once:\n%s", n, got)
	}
	// A tab would be read as a table cell and shift the text.
	if strings.Contains(got, "\t") {
		t.Errorf("output has a tab:\n%s", got)
	}
}

func TestARouterTryPrintsEveryDestinationsShare(t *testing.T) {
	try := control.RouterTry{Router: "auto", Routes: []gateway.RouteTry{{
		Step: "route", Mode: "instruction", Model: "keera-decider",
		Destinations: []string{"keera-speed", "keera-frontier"},
		Trials: []gateway.RouteTrial{{
			Prompt: "plan a migration", Chosen: "keera-frontier", Confidence: 0.62,
			Shares: []float64{0.38, 0.62}, Read: "probabilities",
			Asked: gateway.Asked{Calls: []gateway.ModelCall{call("Choose.", `["plan a migration"]`, "B")}},
		}},
	}}}

	var buf bytes.Buffer
	w := newTable(&buf)
	printRouterTry(w, try)
	_ = w.Flush()
	got := buf.String()

	for _, want := range []string{"keera-frontier, 62% sure", "keera-speed", "38%", "it answered:", "B"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
