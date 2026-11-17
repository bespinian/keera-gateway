package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// liveTeam are the people whose keys send the live requests, when their
// projects may use the model. Their projects run no filter on a model, so a
// request does not wait on the decider. When none of them may, everyone who
// may sends them.
var liveTeam = []string{"Mia Schneider", "Jonas Weber", "Nina Frei"}

// sender is a person who sends live requests, and their key.
type sender struct {
	member
	key string
}

var openings = []string{
	"Write a Go function that checks whether a string is a valid Swiss IBAN.",
	"Suggest a commit message for: rename tmp2 to total in invoice.go",
	"How do I add a nullable column in Postgres without locking the table for long?",
	"Explain the difference between context.WithTimeout and context.WithDeadline in two sentences.",
	"Give the variable x a clearer name: for _, x := range orders { sum += x.Amount }",
	"Write a unit test for a function that formats CHF amounts with two decimals.",
	"Which HTTP status should an API return when a budget is used up?",
	"What does this regular expression match: ^CH\\d{2}[0-9A-Z]{17}$",
	"Turn this into a table-driven test: if add(1, 2) != 3 { t.Fail() }",
	"Why would a React component render twice in development?",
}

var followUps = []string{
	"Make it shorter.",
	"Now add error handling.",
	"Explain the second line.",
	"What about edge cases?",
	"Write it in TypeScript instead.",
}

// live sends small requests to model through the gateway until ctx ends, so
// the live map and the request log move during a demo. Each is a short
// conversation with one person's key, its turns a second or two apart.
func (d *demo) live(ctx context.Context, gateway, model string) error {
	senders, err := d.senders(ctx, model)
	if err != nil {
		return err
	}
	names := make([]string, len(senders))
	for i, s := range senders {
		names[i] = s.keyName()
	}
	fmt.Printf("\nSending requests to %s through %s, with %s. Ctrl+C stops.\n",
		model, gateway, strings.Join(names, ", "))
	client := &http.Client{Timeout: 2 * time.Minute}
	// A few conversations at once, so requests overlap as they do in a team.
	var wg sync.WaitGroup
	for range min(liveConversations, len(senders)) {
		wg.Go(func() { converse(ctx, client, gateway, model, senders) })
	}
	wg.Wait()
	return nil
}

// liveConversations is how many conversations run at once. More would only
// queue on a CPU backend, which answers one request at a time.
const liveConversations = 2

// converse runs one conversation after another, each with a random sender,
// until ctx ends.
func converse(ctx context.Context, client *http.Client, gateway, model string, senders []sender) {
	rnd := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	pause := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(500+rnd.IntN(2500)) * time.Millisecond):
			return true
		}
	}
	for pause() {
		m := senders[rnd.IntN(len(senders))]
		messages := []chatMessage{{"user", openings[rnd.IntN(len(openings))]}}
		for turn := range 1 + rnd.IntN(3) {
			if turn > 0 {
				if !pause() {
					return
				}
				messages = append(messages, chatMessage{"user", followUps[rnd.IntN(len(followUps))]})
			}
			began := time.Now()
			reply, status, err := ask(ctx, client, gateway, m.key, m.client, model, messages)
			if ctx.Err() != nil {
				return
			}
			line := fmt.Sprintf("%s  %-15s %d  %.1fs", began.Format("15:04:05"), m.keyName(),
				status, time.Since(began).Seconds())
			if err != nil {
				fmt.Printf("%s  %v\n", line, err)
				break
			}
			fmt.Println(line)
			messages = append(messages, chatMessage{"assistant", reply})
		}
	}
}

// senders finds the people whose keys may call model. A key made in this run
// is used as it is. The secret of an older one was shown once and is gone, so
// the key gets a new one: it keeps its name, project and history.
func (d *demo) senders(ctx context.Context, model string) ([]sender, error) {
	may, err := d.mayCall(ctx, model)
	if err != nil {
		return nil, err
	}
	var all, preferred []sender
	for _, m := range team {
		var keyID string
		err := d.st.Pool().QueryRow(ctx, `SELECT id FROM api_keys
			WHERE org_id = $1 AND name = $2 AND key_hash IS NOT NULL AND revoked_at IS NULL
				AND (expires_at IS NULL OR expires_at > now())
			ORDER BY created_at DESC LIMIT 1`, d.orgID, m.keyName()).Scan(&keyID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r, err := d.st.LookupKeyByID(ctx, keyID)
		if err != nil {
			return nil, err
		}
		if !may(r) {
			continue
		}
		key, ok := d.secrets[m.keyName()]
		if !ok {
			var hash []byte
			var prefix string
			key, hash, prefix = auth.Generate()
			if _, err := d.st.Pool().Exec(ctx,
				"UPDATE api_keys SET key_hash = $2, prefix = $3 WHERE id = $1",
				keyID, hash, prefix); err != nil {
				return nil, err
			}
		}
		s := sender{m, key}
		all = append(all, s)
		if slices.Contains(liveTeam, m.name) {
			preferred = append(preferred, s)
		}
	}
	switch {
	case len(preferred) > 0:
		return preferred, nil
	case len(all) > 0:
		return all, nil
	}
	return nil, fmt.Errorf("no demo person's key may call %s. "+
		"The organisation was not made by this script, or its guardrails keep %s out", model, model)
}

// mayCall returns whether a key may call the model or router named alias.
func (d *demo) mayCall(ctx context.Context, alias string) (func(*policy.Resolved) bool, error) {
	models, err := d.st.ListModels(ctx, d.orgID)
	if err != nil {
		return nil, err
	}
	find := func(a string) (policy.Model, bool) {
		i := slices.IndexFunc(models, func(m policy.Model) bool { return m.Alias == a })
		if i < 0 {
			return policy.Model{}, false
		}
		return models[i], true
	}
	if m, ok := find(alias); ok {
		return func(r *policy.Resolved) bool { return r.MayUse(m) }, nil
	}
	routers, err := d.st.ListRouters(ctx, d.orgID)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(routers, func(p policy.Router) bool { return p.Alias == alias }); i >= 0 {
		return func(r *policy.Resolved) bool { return r.MayCallRouter(routers[i], find) }, nil
	}
	return nil, fmt.Errorf("the organisation has no model or router called %q", alias)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ask sends one chat completion and returns the answer's text, or what the
// gateway said went wrong.
func ask(ctx context.Context, client *http.Client, gateway, key, clientName, model string,
	messages []chatMessage,
) (reply string, status int, err error) {
	body, err := json.Marshal(map[string]any{
		"model": model, "messages": messages,
		// Short answers keep a CPU backend quick.
		"max_tokens": 64,
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(gateway, "/")+"/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// The client the key's holder uses, so the live map shows it.
	req.Header.Set("X-Keera-Client", clientName)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", resp.StatusCode, fmt.Errorf("the answer is not JSON: %.200s", raw)
	}
	switch {
	case resp.StatusCode >= 300:
		return "", resp.StatusCode, errors.New(cmp.Or(out.Error.Message, resp.Status))
	case len(out.Choices) == 0:
		return "", resp.StatusCode, errors.New("the answer has no choices")
	}
	return out.Choices[0].Message.Content, resp.StatusCode, nil
}
