package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A decision model is a classifier, such as Laya behind laya-serve. It reads a
// text and a typed question, and answers with a probability for each answer.
// It writes no text, so it can decide for a router or a gate and nothing else.
//
// It is asked on /systemone, the endpoint laya-serve and TypeSafe Jev share.
// Its answer is read back into the chat completion a router or a gate would
// have had from a chat model, with the probabilities as the first token's
// logprobs. So both read one shape, and a decision is read the same way
// whatever kind of model made it.

// decisionPath is where a decision model is asked, under its backend's base
// URL.
const decisionPath = "/systemone"

// decisionID is the one question a request asks. The answer is looked up
// under it.
const decisionID = "decision"

// decisionQuestion is one typed question, in the shape /systemone takes.
type decisionQuestion struct {
	// Type is "noul" for yes or no, and "choice" for one of several labels.
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// text is all the question's words, to measure what a decision model is sent.
func (q decisionQuestion) text() string {
	parts := []string{q.Instructions}
	for _, c := range q.Criteria {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n")
}

// gateDecision is a gate's instruction as a yes-or-no question, where yes
// refuses. Lines starting 'yes:' and 'no:' say what each answer means; a
// classifier is surer with them, and the rest is the question.
func gateDecision(prompt string) decisionQuestion {
	q := decisionQuestion{Type: "noul"}
	var question []string
	for line := range strings.SplitSeq(prompt, "\n") {
		label, text, ok := strings.Cut(line, ":")
		answer := map[string]string{"yes": "true", "no": "false"}[strings.ToLower(strings.TrimSpace(label))]
		if ok && answer != "" {
			if q.Criteria == nil {
				q.Criteria = map[string]string{}
			}
			q.Criteria[answer] = strings.TrimSpace(text)
			continue
		}
		question = append(question, line)
	}
	q.Instructions = strings.TrimSpace(strings.Join(question, "\n"))
	return q
}

// routerDecision is a router's instruction as a choice between the offered
// destinations, each under its letter and described by its model's
// description. The alias is left out when there is a description: measured on
// Laya, it only adds words that say nothing about the request.
func (s *Server) routerDecision(orgID, prompt string, offered []string) decisionQuestion {
	q := decisionQuestion{Type: "choice", Instructions: strings.TrimSpace(prompt),
		Criteria: make(map[string]string, len(offered))}
	for i, alias := range offered {
		about := alias
		if m, _ := s.src.Model(orgID, alias); m.Description != "" {
			about = m.Description
		}
		q.Criteria[string(routerLabels[i])] = about
	}
	return q
}

// decisionRequest is the /systemone request for one question about the
// segments.
//
// max_len is the model's context, so the classifier reads exactly what the
// gateway measured against it. Left to the server, it would cut a longer
// request short and decide on part of it, which a gate must never do. Half of
// it is room for the question; the question only takes what it needs.
func decisionRequest(m policy.Model, q decisionQuestion, input []byte) ([]byte, error) {
	return json.Marshal(decisionBody(m, q, json.RawMessage(input)))
}

// decisionBody is decisionRequest before it is encoded, for a model check.
func decisionBody(m policy.Model, q decisionQuestion, state any) map[string]any {
	body := map[string]any{
		"state":     state,
		"questions": map[string]decisionQuestion{decisionID: q},
	}
	if m.BackendModel != "" {
		body["model"] = m.BackendModel
	}
	// max_len is the model's context, so the classifier reads exactly what the
	// gateway measured. Left to the server, it would cut a longer request short
	// and decide on part of it, which a gate must never do. Half is room for the
	// question.
	if m.MaxContext > 0 {
		body["max_len"] = m.MaxContext
		body["head_max_len"] = m.MaxContext / 2
	}
	return body
}

// decisionReply is the part of a /systemone answer that is read.
type decisionReply struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		// Noul is the probability of yes.
		Noul *float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// chatFromDecision turns a /systemone answer into the chat completion a router
// or a gate reads: the answer as content, and every answer's probability as
// the first token's logprobs.
//
// A gate refuses when yes is more likely than no, which is what reading the
// same distribution as logprobs decides too.
func chatFromDecision(raw []byte, q decisionQuestion) ([]byte, error) {
	var r decisionReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("its answer is not a decision: %w", err)
	}
	a, ok := r.Answers[decisionID]
	if !ok {
		return nil, errors.New("its answer has no decision in it")
	}

	var content string
	var dist map[string]float64
	switch q.Type {
	case "noul":
		if a.Noul == nil {
			return nil, errors.New("its answer has no probability for yes or no")
		}
		dist = map[string]float64{"ALLOW": 1 - *a.Noul, filterRefusalToken: *a.Noul}
		content = "ALLOW"
		if *a.Noul > 0.5 {
			content = fmt.Sprintf("%s: the decision model judged it to be what this gate "+
				"refuses (%.0f%% sure)", filterRefusalToken, *a.Noul*100)
		}
	default:
		if _, ok := q.Criteria[a.Choice]; !ok {
			return nil, fmt.Errorf("it chose %q, which it was not offered", a.Choice)
		}
		dist, content = a.Probabilities, a.Choice
	}

	type logprob struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	}
	top := make([]logprob, 0, len(dist))
	for token, p := range dist {
		// A probability rounded to 0 has no logarithm.
		top = append(top, logprob{token, math.Log(max(p, 1e-9))})
	}
	word, _, _ := strings.Cut(content, ":")
	first := map[string]any{"token": word, "logprob": math.Log(max(dist[word], 1e-9)),
		"top_logprobs": top}
	return json.Marshal(map[string]any{
		"object": "chat.completion",
		"model":  r.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": content},
			"finish_reason": "stop",
			"logprobs":      map[string]any{"content": []any{first}},
		}},
		"usage": map[string]int64{
			"prompt_tokens":     r.Usage.InputTokens,
			"completion_tokens": r.Usage.OutputTokens,
			"total_tokens":      r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	})
}
