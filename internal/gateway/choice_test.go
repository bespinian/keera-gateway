package gateway

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// completion builds a completion with one token and its logprob alternatives.
func completion(sampled string, alternatives ...[2]any) string {
	tops := make([]string, 0, len(alternatives))
	for _, alt := range alternatives {
		tops = append(tops, fmt.Sprintf(`{"token":%q,"logprob":%v}`, alt[0], alt[1]))
	}
	var sampledLogprob any = -0.1
	if len(alternatives) > 0 {
		sampledLogprob = alternatives[0][1]
	}
	return fmt.Sprintf(`{"choices":[{"message":{"content":%q},"logprobs":{"content":[`+
		`{"token":%q,"logprob":%v,"top_logprobs":[%s]}]}}]}`,
		sampled, sampled, sampledLogprob, strings.Join(tops, ","))
}

// The answer is whichever offered letter took the most probability.
func TestAChoiceIsReadOutOfTheLettersAlone(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		n     int
		want  int
		share float64
	}{
		{
			name: "the letter the model chose",
			raw:  completion("B", [2]any{"B", -0.01}, [2]any{"A", -5.0}),
			n:    2, want: 1, share: 0.99,
		},
		{
			// A model that opens with '<think>' still chooses.
			name: "a token that is not a letter is not in the running",
			raw: completion("<think>", [2]any{"<think>", -0.1}, [2]any{"A", -4.0},
				[2]any{"B", -6.0}),
			n: 2, want: 0, share: 0.88,
		},
		{
			name: "the letter behind a space, which is how most vocabularies spell it",
			raw:  completion(" A", [2]any{" A", -0.01}, [2]any{" B", -5.0}),
			n:    2, want: 0, share: 0.99,
		},
		{
			name: "a model that answered in lower case answered",
			raw:  completion("b", [2]any{"b", -0.01}, [2]any{"a", -5.0}),
			n:    2, want: 1, share: 0.99,
		},
		{
			// Two spellings of one answer add up, or a third letter could win.
			name: "both spellings of a letter count for it",
			raw: completion("A", [2]any{"A", -1.0}, [2]any{"a", -1.0},
				[2]any{"B", -0.9}),
			n: 2, want: 0, share: 0.64,
		},
		{
			// A destination that was not offered cannot be chosen.
			name: "a letter past the end of the list is not a destination",
			raw:  completion("C", [2]any{"C", -0.01}, [2]any{"A", -5.0}),
			n:    2, want: 0, share: 1,
		},
		{
			// 'Absolutely' is not a vote for A.
			name: "a word starting with a letter is not that letter",
			raw:  completion("Absolutely", [2]any{"Absolutely", -0.01}),
			n:    2, want: -1,
		},
		{
			name: "a plane that serves no distribution",
			raw:  `{"choices":[{"message":{"content":"keera-large"}}]}`,
			n:    2, want: -1,
		},
		{
			name: "nothing that is a completion at all",
			raw:  `not json`,
			n:    2, want: -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			i, share, ok := readChoice([]byte(tc.raw), tc.n)
			if tc.want < 0 {
				if ok {
					t.Fatalf("read a choice of %d out of an answer that holds none", i)
				}
				return
			}
			if !ok {
				t.Fatal("no choice was read")
			}
			if i != tc.want {
				t.Errorf("chose %d, want %d", i, tc.want)
			}
			if math.Abs(share-tc.share) > 0.02 {
				t.Errorf("share = %.3f, want about %.3f", share, tc.share)
			}
		})
	}
}

// The shares are a distribution over the offered destinations, not raw model
// output.
func TestTheSharesOfAChoiceAddUp(t *testing.T) {
	raw := completion("A", [2]any{"A", -0.7}, [2]any{"B", -0.9}, [2]any{"C", -2.5})
	_, share, ok := readChoice([]byte(raw), 3)
	if !ok {
		t.Fatal("no choice was read")
	}
	// The model can barely tell three destinations apart, so the winner's
	// share is low.
	if share < 0.3 || share > 0.6 {
		t.Errorf("share = %.3f, want the winner of a close three-way choice", share)
	}
}

// The same tie must resolve the same way each time, not by map order.
func TestATieIsBrokenTheSameWayEveryTime(t *testing.T) {
	raw := completion("A", [2]any{"A", -1.0}, [2]any{"B", -1.0}, [2]any{"C", -1.0})
	for range 50 {
		i, _, ok := readChoice([]byte(raw), 3)
		if !ok || i != 0 {
			t.Fatalf("a tie was read as %d (ok=%v), want the first destination", i, ok)
		}
	}
}

// A router with more destinations than letters must not lose any.
func TestTooManyDestinationsAreNotLettered(t *testing.T) {
	if labelled(nil) {
		t.Error("a router with nothing to offer was lettered")
	}
	if !labelled(make([]string, len(choiceLetters))) {
		t.Error("a router with as many destinations as there are letters was not lettered")
	}
	if labelled(make([]string, len(choiceLetters)+1)) {
		t.Error("a router with more destinations than letters was lettered anyway")
	}
}

// ALLOW and REFUSED begin differently, so any token prefix tells them apart.
func TestAVerdictIsReadOffTheWordsAGateAlreadyAnswers(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		allow bool
		share float64
		read  bool
	}{
		{
			name:  "the whole word",
			raw:   completion("ALLOW", [2]any{"ALLOW", -0.01}, [2]any{"REFUSED", -5.0}),
			allow: true, share: 0.99, read: true,
		},
		{
			// A tokeniser may split the word anywhere.
			name:  "however the tokeniser split it",
			raw:   completion("REF", [2]any{"REF", -0.01}, [2]any{"AL", -5.0}),
			allow: false, share: 0.99, read: true,
		},
		{
			name:  "one letter of it",
			raw:   completion("R", [2]any{"R", -0.02}, [2]any{"A", -4.0}),
			allow: false, share: 0.98, read: true,
		},
		{
			name:  "more than the word",
			raw:   completion("ALLOWED", [2]any{"ALLOWED", -0.01}, [2]any{"REFUSE", -5.0}),
			allow: true, share: 0.99, read: true,
		},
		{
			// A model that thinks first still gives a verdict.
			name: "a model that opens with something else entirely",
			raw: completion("<think>", [2]any{"<think>", -0.05}, [2]any{"ALLOW", -3.0},
				[2]any{"REFUSED", -7.0}),
			allow: true, share: 0.98, read: true,
		},
		{
			// 'Absolutely' is not ALLOW.
			name: "a word that merely starts with the same letter",
			raw:  completion("Absolutely", [2]any{"Absolutely", -0.01}),
			read: false,
		},
		{
			name: "a plane that serves no distribution",
			raw:  `{"choices":[{"message":{"content":"ALLOW"}}]}`,
			read: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allow, share, ok := readVerdict([]byte(tc.raw))
			if ok != tc.read {
				t.Fatalf("read = %v, want %v", ok, tc.read)
			}
			if !tc.read {
				return
			}
			if allow != tc.allow {
				t.Errorf("allow = %v, want %v", allow, tc.allow)
			}
			if math.Abs(share-tc.share) > 0.02 {
				t.Errorf("share = %.3f, want about %.3f", share, tc.share)
			}
		})
	}
}

// A gate that can barely tell the two answers apart must show it.
func TestAVerdictReportsWhenItWasNearlyAToss(t *testing.T) {
	raw := completion("ALLOW", [2]any{"ALLOW", -0.65}, [2]any{"REFUSED", -0.75})
	allow, share, ok := readVerdict([]byte(raw))
	if !ok || !allow {
		t.Fatalf("allow = %v, ok = %v, want an allow", allow, ok)
	}
	if share > 0.6 {
		t.Errorf("share = %.3f, want a verdict that reads as barely made", share)
	}
}
