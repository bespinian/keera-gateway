package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// `keera task` is the work people gave Keera Agents' agents: from a chat, a
// schedule, a card, a file or 'keera agent ask'.

type taskRun struct {
	*cmdRun
	agent, state, source string
	limit                int
	follow               bool
}

func taskCmd(ctx context.Context, args []string) error {
	r := &taskRun{cmdRun: newCmdRun("task", args)}
	fs := r.fs
	fs.StringVar(&r.agent, "agent", "", "only this agent's, by name or id")
	fs.StringVar(&r.state, "state", "", "only queued, running, succeeded, failed or cancelled ones")
	fs.StringVar(&r.source, "source", "", "only those from chat, schedule, board, file or manual")
	fs.IntVar(&r.limit, "limit", reportLimit, "how many to print, up to 200")
	fs.BoolVar(&r.follow, "follow", false, "keep printing its steps until it ends")
	if done, err := r.parse(); done {
		return err
	}
	if r.follow && r.asJSON {
		return errors.New("--follow prints as the agent works; leave out --json")
	}
	a, err := connectAgents(ctx, r.c)
	if err != nil {
		return err
	}
	switch r.verb {
	case "show":
		return r.show(ctx, a)
	case "cancel":
		return r.cancel(ctx, a)
	default:
		return r.list(ctx, a)
	}
}

// taskEntry is a task as Keera Agents answers it.
type taskEntry struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	AgentName   string     `json:"agent_name"`
	Source      string     `json:"source"`
	OriginLabel string     `json:"origin_label"`
	Prompt      string     `json:"prompt"`
	RequestedBy string     `json:"requested_by"`
	State       string     `json:"state"`
	Result      string     `json:"result"`
	Error       string     `json:"error"`
	Delivered   bool       `json:"delivered"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

func (t taskEntry) finished() bool {
	return t.State == "succeeded" || t.State == "failed" || t.State == "cancelled"
}

// taskEvent is one step of a task: a message, a tool call and its result,
// or a line of log.
type taskEvent struct {
	ID   int64           `json:"id"`
	Kind string          `json:"kind"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data,omitempty"`
	At   time.Time       `json:"at"`
}

func taskPath(id string) string { return "/api/v1/tasks/" + url.PathEscape(id) }

func (r *taskRun) list(ctx context.Context, a *agents) error {
	if r.limit < 1 || r.limit > 200 {
		return errors.New("--limit takes 1 to 200")
	}
	q := url.Values{"limit": {strconv.Itoa(r.limit)}}
	setIfGiven(q, map[string]string{"state": r.state, "source": r.source})
	if r.agent != "" {
		e, err := findAgent(ctx, a, r.agent)
		if err != nil {
			return err
		}
		q.Set("agent_id", e.ID)
	}
	tasks, err := list[taskEntry](ctx, a, "/api/v1/tasks?"+q.Encode())
	if err != nil {
		return err
	}
	return out(r.asJSON, tasks, func(w *table) {
		if len(tasks) == 0 {
			printNone(w, "tasks", "keera agent ask <agent> <prompt>")
			return
		}
		w.header("TASK\tCREATED\tAGENT\tSTATE\tFROM\tASKED BY\tPROMPT")
		for _, t := range tasks {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID,
				t.CreatedAt.Local().Format("2006-01-02 15:04"), dash(t.AgentName), statusWord(t.State),
				taskFrom(t), dash(t.RequestedBy), oneLine(firstLine(t.Prompt)))
		}
		if len(tasks) == r.limit {
			_, _ = fmt.Fprintf(w, "\nShowing the newest %d; raise --limit for more.\n", r.limit)
		}
	})
}

// taskFrom is where it came from, such as "Slack #sales", or its source.
func taskFrom(t taskEntry) string {
	if t.OriginLabel != "" {
		return t.OriginLabel
	}
	return dash(t.Source)
}

func (r *taskRun) show(ctx context.Context, a *agents) error {
	var res struct {
		taskEntry
		Events []taskEvent `json:"events"`
	}
	if err := a.do(ctx, "GET", taskPath(r.fs.Arg(0)), nil, &res); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, res, nil)
	}
	t := res.taskEntry
	if err := printTaskHead(os.Stdout, t); err != nil {
		return err
	}
	var last int64
	for _, e := range res.Events {
		printEvent(os.Stdout, e)
		last = e.ID
	}
	if r.follow && !t.finished() {
		return followFrom(ctx, a, t, last, os.Stdout)
	}
	printTaskEnd(os.Stdout, t)
	return nil
}

func printTaskHead(w io.Writer, t taskEntry) error {
	tw := newTable(w)
	show(tw, "task", t.ID)
	show(tw, "agent", dash(t.AgentName))
	show(tw, "state", statusWord(t.State))
	show(tw, "from", taskFrom(t))
	show(tw, "asked by", dash(t.RequestedBy))
	show(tw, "created", t.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	if t.StartedAt != nil && t.FinishedAt != nil {
		show(tw, "took", shortDuration(t.FinishedAt.Sub(*t.StartedAt)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "\n%s\n%s\n\n", style.head("Prompt:"), indent(t.Prompt, "  "))
	return nil
}

// printEvent prints a step as the panel shows it folded: messages whole,
// tool calls and results by their first line.
func printEvent(w io.Writer, e taskEvent) {
	at := style.muted(e.At.Local().Format("15:04:05"))
	text := strings.TrimSpace(e.Text)
	switch e.Kind {
	case "message":
		// Lines after the first line up under it, past the time.
		first, rest, _ := strings.Cut(text, "\n")
		_, _ = fmt.Fprintf(w, "%s  %s\n", at, first)
		if rest != "" {
			_, _ = fmt.Fprintln(w, indent(rest, "          "))
		}
	case "tool_call":
		_, _ = fmt.Fprintf(w, "%s  %s %s\n", at, style.cmd("tool call"), firstLine(text))
	case "tool_result":
		_, _ = fmt.Fprintf(w, "%s  %s %s\n", at, style.muted("result"), firstLine(text))
	default:
		_, _ = fmt.Fprintf(w, "%s  %s\n", at, style.muted(oneLine(text)))
	}
}

// printTaskEnd prints the answer, or why there is none.
func printTaskEnd(w io.Writer, t taskEntry) {
	switch {
	case t.Error != "":
		_, _ = fmt.Fprintf(w, "\n%s %s\n", style.bad(t.State+":"), t.Error)
	case t.Result != "":
		_, _ = fmt.Fprintf(w, "\n%s\n%s\n", style.head("Answer:"), t.Result)
	case !t.finished():
		_, _ = fmt.Fprintf(w, "\nIt is %s. Follow it with: keera task show %s --follow\n", t.State, t.ID)
	}
}

// followTask prints a task's steps from its first until it ends.
func followTask(ctx context.Context, a *agents, t taskEntry, w io.Writer) error {
	if err := printTaskHead(w, t); err != nil {
		return err
	}
	return followFrom(ctx, a, t, 0, w)
}

// pollEvery is how often a followed task is asked for new steps.
var pollEvery = 2 * time.Second

// followFrom prints the steps after the one numbered after until the task
// ends, then its answer. Ctrl-C stops following; the task goes on.
func followFrom(ctx context.Context, a *agents, t taskEntry, after int64, w io.Writer) error {
	for {
		var res struct {
			Events []taskEvent `json:"events"`
			Task   taskEntry   `json:"task"`
		}
		path := taskPath(t.ID) + "/events?after=" + strconv.FormatInt(after, 10)
		if err := a.do(ctx, "GET", path, nil, &res); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, e := range res.Events {
			printEvent(w, e)
			after = e.ID
		}
		if res.Task.finished() {
			printTaskEnd(w, res.Task)
			return nil
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(os.Stderr, "\nStopped following; %s goes on.\n", t.ID)
			return nil
		case <-time.After(pollEvery):
		}
	}
}

func (r *taskRun) cancel(ctx context.Context, a *agents) error {
	var t taskEntry
	if err := a.do(ctx, "POST", taskPath(r.fs.Arg(0))+"/cancel", struct{}{}, &t); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, t, nil)
	}
	fmt.Printf("Cancelled %s.\n", t.ID)
	return nil
}
