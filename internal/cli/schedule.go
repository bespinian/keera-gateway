package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"
)

// `keera schedule` gives an agent the same task on a timetable, such as
// every weekday at nine.

type scheduleRun struct {
	*cmdRun
	agent, name, cron, timezone, prompt string
	wait                                bool
}

func scheduleCmd(ctx context.Context, args []string) error {
	r := &scheduleRun{cmdRun: newCmdRun("schedule", args)}
	fs := r.fs
	fs.StringVar(&r.agent, "agent", "", "the agent, by name or id")
	r.reword("list", "agent", "only this agent's, by name or id")
	r.reword("create", "agent", "the agent that does it, by name or id (required)")
	fs.StringVar(&r.name, "name", "", "a new name")
	fs.StringVar(&r.cron, "cron", "", `when, as a cron expression such as "0 9 * * 1-5"`)
	r.reword("create", "cron", `when, as a cron expression such as "0 9 * * 1-5" (required)`)
	fs.StringVar(&r.timezone, "timezone", "", "the time zone the expression is read in, such as Europe/Zurich")
	fs.StringVar(&r.prompt, "prompt", "", "what the agent should do each time; @file reads a file")
	r.reword("create", "prompt", "what the agent should do each time; @file reads a file (required)")
	fs.BoolVar(&r.wait, "wait", false, "print the task's steps as it works, and its answer at the end")
	if done, err := r.parse(); done {
		return err
	}
	if r.wait && r.asJSON {
		return errors.New("--wait prints as the agent works; leave out --json")
	}
	a, err := connectAgents(ctx, r.c)
	if err != nil {
		return err
	}
	switch r.verb {
	case "create":
		return r.create(ctx, a)
	case "set":
		return r.set(ctx, a)
	case "enable", "disable":
		return r.enable(ctx, a)
	case "run":
		return r.run(ctx, a)
	case "delete":
		return r.delete(ctx, a)
	case "preview":
		return r.preview(ctx, a)
	default:
		return r.list(ctx, a)
	}
}

// scheduleEntry is a schedule as Keera Agents answers it. Description is
// the expression in words.
type scheduleEntry struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	AgentName   string     `json:"agent_name"`
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	Description string     `json:"description"`
	Timezone    string     `json:"timezone"`
	Prompt      string     `json:"prompt"`
	Enabled     bool       `json:"enabled"`
	NextRunAt   *time.Time `json:"next_run_at"`
	LastRunAt   *time.Time `json:"last_run_at"`
	RequestedBy string     `json:"requested_by"`
}

func schedulePath(id string) string { return "/api/v1/schedules/" + url.PathEscape(id) }

// findSchedule takes a schedule's id or name.
func findSchedule(ctx context.Context, a *agents, given string) (scheduleEntry, error) {
	list, err := listOf[scheduleEntry](ctx, a, "/api/v1/schedules")
	if err != nil {
		return scheduleEntry{}, err
	}
	return pickOne(list, given, "schedule", "schedule list", func(s scheduleEntry) (string, string) {
		return s.ID, s.Name
	})
}

func (r *scheduleRun) list(ctx context.Context, a *agents) error {
	path := "/api/v1/schedules"
	if r.agent != "" {
		e, err := findAgent(ctx, a, r.agent)
		if err != nil {
			return err
		}
		path += "?agent_id=" + url.QueryEscape(e.ID)
	}
	list, err := listOf[scheduleEntry](ctx, a, path)
	if err != nil {
		return err
	}
	return out(r.asJSON, list, func(w *table) {
		if len(list) == 0 {
			printNone(w, "schedules", `keera schedule create <name> --agent <agent> --cron "0 9 * * 1-5" --prompt <prompt>`)
			return
		}
		printSchedules(w, true, list...)
	})
}

// printSchedules names each one's agent when they are not all one agent's.
func printSchedules(w *table, withAgent bool, list ...scheduleEntry) {
	head := "NAME\tWHEN\tNEXT RUN\tID"
	if withAgent {
		head = "NAME\tAGENT\tWHEN\tNEXT RUN\tID"
	}
	w.header(head)
	for _, s := range list {
		next := localTime(s.NextRunAt)
		if !s.Enabled {
			next = statusWord("disabled")
		}
		when := s.Description
		if s.Timezone != "" {
			when += " (" + s.Timezone + ")"
		}
		if withAgent {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.AgentName, when, next, s.ID)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, when, next, s.ID)
		}
	}
}

// body is what create and set send: only the fields given.
func (r *scheduleRun) body() (map[string]any, error) {
	b := map[string]any{}
	for name, v := range map[string]string{"name": r.name, "cron": r.cron, "timezone": r.timezone} {
		if given(r.fs, name) {
			b[name] = v
		}
	}
	if given(r.fs, "prompt") {
		text, err := textOrFile(r.prompt)
		if err != nil {
			return nil, err
		}
		b["prompt"] = text
	}
	return b, nil
}

func (r *scheduleRun) create(ctx context.Context, a *agents) error {
	for _, f := range []string{"agent", "cron", "prompt"} {
		if !given(r.fs, f) {
			return fmt.Errorf("--%s is required (see: keera help schedule create)", f)
		}
	}
	e, err := findAgent(ctx, a, r.agent)
	if err != nil {
		return err
	}
	b, err := r.body()
	if err != nil {
		return err
	}
	b["name"], b["agent_id"] = r.fs.Arg(0), e.ID
	var s scheduleEntry
	if err := a.do(ctx, "POST", "/api/v1/schedules", b, &s); err != nil {
		return err
	}
	return out(r.asJSON, s, func(w *table) { printSchedules(w, true, s) })
}

func (r *scheduleRun) set(ctx context.Context, a *agents) error {
	if !changesSomething(r.fs) {
		return nothingToChange("schedule set")
	}
	s, err := findSchedule(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := r.body()
	if err != nil {
		return err
	}
	return r.patch(ctx, a, s, b)
}

func (r *scheduleRun) enable(ctx context.Context, a *agents) error {
	s, err := findSchedule(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return r.patch(ctx, a, s, map[string]any{"enabled": r.verb == "enable"})
}

func (r *scheduleRun) patch(ctx context.Context, a *agents, s scheduleEntry, b map[string]any) error {
	var changed scheduleEntry
	if err := a.do(ctx, "PATCH", schedulePath(s.ID), b, &changed); err != nil {
		return err
	}
	return out(r.asJSON, changed, func(w *table) { printSchedules(w, true, changed) })
}

// run gives the agent the schedule's task now, as its next run would.
func (r *scheduleRun) run(ctx context.Context, a *agents) error {
	s, err := findSchedule(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var t taskEntry
	if err := a.do(ctx, "POST", schedulePath(s.ID)+"/run", struct{}{}, &t); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, t, nil)
	}
	if r.wait {
		return followTask(ctx, a, t, os.Stdout)
	}
	fmt.Printf("Gave %s the task %s. Follow it with:\n\n  keera task show %s --follow\n", s.AgentName, t.ID, t.ID)
	return nil
}

func (r *scheduleRun) delete(ctx context.Context, a *agents) error {
	s, err := findSchedule(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirm("Delete the schedule "+s.Name+"?", []string{
			s.AgentName + " is no longer given its task. The tasks it gave stay.",
		}, "schedule's name", s.Name, "nothing was deleted"); err != nil {
			return err
		}
	}
	if err := a.do(ctx, "DELETE", schedulePath(s.ID), nil, nil); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, map[string]string{"deleted": s.ID}, nil)
	}
	fmt.Printf("Deleted %s.\n", s.Name)
	return nil
}

// preview says when an expression runs, before a schedule uses it.
func (r *scheduleRun) preview(ctx context.Context, a *agents) error {
	var res struct {
		Description string      `json:"description"`
		Next        []time.Time `json:"next"`
	}
	in := map[string]string{"cron": r.fs.Arg(0), "timezone": r.timezone}
	if err := a.do(ctx, "POST", "/api/v1/schedules/preview", in, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintln(w, res.Description)
		_, _ = fmt.Fprintln(w, "\nThe next runs, in this machine's time:")
		for _, t := range res.Next {
			_, _ = fmt.Fprintf(w, "  %s\n", t.Local().Format("Mon 2006-01-02 15:04"))
		}
	})
}
