package cli

// Keera Agents is a separate app next to the gateway: agents that take
// tasks, their schedules, and workspaces. /v1/me names its address, and it
// takes the same sign-in, so 'keera agent', 'task', 'schedule' and
// 'workspace' reach it after 'keera login'.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// agents is Keera Agents, reached with this machine's sign-in.
type agents struct {
	base  string
	token string
	http  *http.Client
}

// connectAgents finds Keera Agents through the gateway. Only a person's
// sign-in works there: the operator key belongs to nobody, and agents and
// workspaces are somebody's.
func connectAgents(ctx context.Context, c *client) (*agents, error) {
	if c.key != "" {
		return nil, errors.New("agents and workspaces need a person's sign-in: " +
			"unset KEERA_OPERATOR_KEY and run 'keera login'")
	}
	token, err := c.credential()
	if err != nil {
		return nil, err
	}
	var me struct {
		AgentsURL string `json:"agents_url"`
	}
	if err := c.do(ctx, "GET", "/v1/me", nil, &me); err != nil {
		return nil, err
	}
	if me.AgentsURL == "" {
		return nil, fmt.Errorf("%s has no Keera Agents connected, so it has no agents or workspaces", c.base)
	}
	return &agents{
		base:  strings.TrimRight(me.AgentsURL, "/"),
		token: token,
		http:  &http.Client{Transport: http1Transport()},
	}, nil
}

// http1Transport is the default transport, but HTTP/1.1 only: a stream takes
// the connection over, which HTTP/2 cannot.
func http1Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	return t
}

func (a *agents) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := jsonRequest(ctx, method, a.base+path, in)
	if err != nil {
		return err
	}
	resp, err := a.send(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readAnswer(resp, a.base)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return agentsError(resp.StatusCode, raw)
	}
	return decode(raw, nil, out)
}

// copyText writes a text answer to w as it arrives, such as a machine's
// logs, until it ends. Ctrl-C ends it too, which is not an error.
func (a *agents) copyText(ctx context.Context, path string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.send(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := readAnswer(resp, a.base)
		return agentsError(resp.StatusCode, raw)
	}
	_, err = io.Copy(w, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// send sends req with this machine's sign-in.
func (a *agents) send(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Keera Agents at %s: %w", a.base, err)
	}
	return resp, nil
}

func agentsError(status int, raw []byte) error {
	if status == http.StatusUnauthorized {
		return errors.New("your sign-in was not accepted by Keera Agents: run 'keera login' again")
	}
	return &apiError{Status: status, Message: errorMessage(raw)}
}

// pickOne finds an entry by id, or else by name ignoring case. Names need
// not be unique, so a name several share is refused rather than guessed.
// listCmd is the command that lists them, such as "agent list".
func pickOne[T any](entries []T, given, noun, listCmd string, idName func(T) (string, string)) (T, error) {
	var zero T
	given = strings.TrimSpace(given)
	var named []T
	for _, e := range entries {
		id, name := idName(e)
		if id == given {
			return e, nil
		}
		if strings.EqualFold(name, given) {
			named = append(named, e)
		}
	}
	switch len(named) {
	case 0:
		return zero, notFoundError(fmt.Sprintf("no %s %q (see: keera %s)", noun, given, listCmd))
	case 1:
		return named[0], nil
	}
	return zero, fmt.Errorf("%d %ss are called %q; name one by its id (see: keera %s)",
		len(named), noun, given, listCmd)
}

// machineClass is the size of an agent's or a workspace's machine.
type machineClass struct {
	Isolation string `json:"isolation"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	Disk      string `json:"disk"`
}

func (c machineClass) String() string {
	parts := []string{}
	if c.CPU != "" {
		parts = append(parts, c.CPU+" CPU")
	}
	if c.Memory != "" {
		parts = append(parts, c.Memory+" memory")
	}
	if c.Disk != "" {
		parts = append(parts, c.Disk+" disk")
	}
	if c.Isolation != "" {
		parts = append(parts, c.Isolation+" isolation")
	}
	return dash(strings.Join(parts, ", "))
}

// machineState says "stopped" for a machine someone stopped, whatever its
// container last reported.
func machineState(state string, enabled bool) string {
	if !enabled {
		return statusWord("stopped")
	}
	return statusWord(dash(state))
}

// machineCost is what one machine cost this month, and an hour of it.
type machineCost struct {
	Month       string `json:"month"`
	MonthMicros int64  `json:"month_micros"`
	HourMicros  int64  `json:"hour_micros"`
}

func printMachineCost(w *table, name string, c machineCost) {
	show(w, "machine", name)
	show(w, "this month ("+c.Month+")", policy.FormatMoney(c.MonthMicros))
	show(w, "an hour running", policy.FormatMoney(c.HourMicros))
	_, _ = fmt.Fprintln(w, "\nModel calls are billed apart, in 'keera usage' and 'keera billing'.")
}

// logsPath asks for a machine's output, and with follow what it prints next.
func logsPath(base string, follow bool) string {
	if follow {
		return base + "/logs?follow=1"
	}
	return base + "/logs"
}

// localTime prints a time the way the rest of the CLI does, or "-".
func localTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
