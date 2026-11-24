package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// `keera agent` works with Keera Agents' agents: models with tools that run
// on a machine of their own and carry out tasks people give them.

type agentRun struct {
	*cmdRun
	name, model, description, instructions string
	tools, people                          stringList
	maxSteps                               int
	access                                 string
	budget                                 float64
	cpu, memory                            string
	follow, wait                           bool
}

func agentCmd(ctx context.Context, args []string) error {
	r := &agentRun{cmdRun: newCmdRun("agent", args)}
	fs := r.fs
	fs.StringVar(&r.name, "name", "", "a new name")
	fs.StringVar(&r.model, "model", "", "the model it works with; 'keera model list' has them")
	r.reword("create", "model", "the model it works with; 'keera model list' has them (required)")
	fs.StringVar(&r.description, "description", "", "what it is for, in a line; empty clears it")
	fs.StringVar(&r.instructions, "instructions", "",
		"how it should work, such as its role and rules; @file reads a file")
	fs.Var(&r.tools, "tools", "its own tools, from shell, files, memory and web; replaces the list")
	fs.IntVar(&r.maxSteps, "max-steps", 0, "the most steps one task may take; 0 is no limit")
	fs.StringVar(&r.access, "access", "", "who may give it work: owner, org or people")
	fs.Var(&r.people, "people", "with --access people, the email addresses that may give it work")
	fs.Float64Var(&r.budget, "budget", 0, "what it may spend on models a month, in CHF; 0 is no budget of its own")
	fs.StringVar(&r.cpu, "cpu", "", "CPUs for its machine, such as 2 or 500m")
	fs.StringVar(&r.memory, "memory", "", "memory for its machine, such as 4Gi")
	fs.BoolVar(&r.follow, "follow", false, "keep printing what it prints next, until Ctrl-C")
	fs.BoolVar(&r.wait, "wait", false, "print the task's steps as it works, and its answer at the end")
	if done, err := r.parse(); done {
		return err
	}
	a, err := connectAgents(ctx, r.c)
	if err != nil {
		return err
	}
	switch r.verb {
	case "show":
		return r.show(ctx, a)
	case "create":
		return r.create(ctx, a)
	case "set":
		return r.set(ctx, a)
	case "delete":
		return r.delete(ctx, a)
	case "start", "stop", "restart", "take-over":
		return r.lifecycle(ctx, a)
	case "ask":
		return r.ask(ctx, a)
	case "logs":
		return r.logs(ctx, a)
	case "cost":
		return r.cost(ctx, a)
	default:
		return r.list(ctx, a)
	}
}

// agentEntry is an agent as Keera Agents answers it.
type agentEntry struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Description   string       `json:"description"`
	Instructions  string       `json:"instructions"`
	Model         string       `json:"model"`
	Builtins      []string     `json:"builtins"`
	MaxSteps      int          `json:"max_steps"`
	Access        string       `json:"access"`
	AllowedEmails []string     `json:"allowed_emails"`
	BudgetMicros  int64        `json:"budget_micros"`
	Class         machineClass `json:"class"`
	State         string       `json:"state"`
	StateDetail   string       `json:"state_detail"`
	Enabled       bool         `json:"enabled"`
	OwnerEmail    string       `json:"owner_email"`
	Queued        int          `json:"queued"`
	RunningTaskID string       `json:"running_task_id"`
	BindingCount  int          `json:"binding_count"`
}

// agentDetail is one agent with what it is connected to.
type agentDetail struct {
	agentEntry
	Bindings []struct {
		ConnectionName string `json:"connection_name"`
		Kind           string `json:"kind"`
		Mode           string `json:"mode"`
		Target         string `json:"target"`
		Label          string `json:"label"`
	} `json:"bindings"`
	Schedules []scheduleEntry `json:"schedules"`
	Stats     struct {
		LastTaskAt *time.Time `json:"last_task_at"`
	} `json:"stats"`
}

func agentPath(id string) string { return "/api/v1/agents/" + url.PathEscape(id) }

// findAgent takes an agent's id or name. Names are unique in an
// organisation, and Keera Agents only takes ids.
func findAgent(ctx context.Context, a *agents, given string) (agentEntry, error) {
	all, err := list[agentEntry](ctx, a, "/api/v1/agents")
	if err != nil {
		return agentEntry{}, err
	}
	return pickOne(all, given, "agent", "agent list", func(e agentEntry) (string, string) { return e.ID, e.Name })
}

func (r *agentRun) list(ctx context.Context, a *agents) error {
	all, err := list[agentEntry](ctx, a, "/api/v1/agents")
	if err != nil {
		return err
	}
	return out(r.asJSON, all, func(w *table) {
		if len(all) == 0 {
			printNone(w, "agents", "keera agent create <name> --model <model>")
			return
		}
		printAgents(w, all...)
	})
}

func printAgents(w *table, list ...agentEntry) {
	w.header("NAME\tSTATE\tMODEL\tTASKS\tOWNER\tID")
	for _, e := range list {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, machineState(e.State, e.Enabled),
			e.Model, agentLoad(e), e.OwnerEmail, e.ID)
	}
}

// agentLoad says whether it is working, and how many tasks wait.
func agentLoad(e agentEntry) string {
	var parts []string
	if e.RunningTaskID != "" {
		parts = append(parts, "1 running")
	}
	if e.Queued > 0 {
		parts = append(parts, strconv.Itoa(e.Queued)+" queued")
	}
	return dash(strings.Join(parts, ", "))
}

func (r *agentRun) show(ctx context.Context, a *agents) error {
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var d agentDetail
	if err := a.do(ctx, "GET", agentPath(e.ID), nil, &d); err != nil {
		return err
	}
	return out(r.asJSON, d, func(w *table) { printAgent(w, d) })
}

func printAgent(w *table, d agentDetail) {
	show(w, "name", d.Name)
	show(w, "id", d.ID)
	show(w, "state", machineState(d.State, d.Enabled))
	showIfSet(w, "description", d.Description)
	show(w, "model", d.Model)
	show(w, "owner", d.OwnerEmail)
	show(w, "who may give it work", accessLabel(d.Access, d.AllowedEmails))
	show(w, "tools", dash(strings.Join(d.Builtins, ", ")))
	show(w, "steps a task", stepsLabel(d.MaxSteps))
	show(w, "budget a month", budgetLabel(d.BudgetMicros))
	show(w, "machine", d.Class)
	show(w, "tasks", agentLoad(d.agentEntry))
	show(w, "last task", localTime(d.Stats.LastTaskAt))
	if d.Instructions != "" {
		show(w, "instructions", firstLine(d.Instructions))
	}
	if d.StateDetail != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", d.StateDetail)
	}
	if len(d.Bindings) > 0 {
		_, _ = fmt.Fprintln(w)
		w.header("CONNECTION\tKIND\tMODE\tWHERE")
		for _, b := range d.Bindings {
			where := b.Target
			if b.Label != "" {
				where = b.Label
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.ConnectionName, b.Kind, b.Mode, where)
		}
	}
	if len(d.Schedules) > 0 {
		_, _ = fmt.Fprintln(w)
		printSchedules(w, false, d.Schedules...)
	}
}

// accessLabel uses the panel's words.
func accessLabel(access string, emails []string) string {
	switch access {
	case "owner":
		return "only its owner"
	case "org":
		return "everyone in the organisation"
	case "people":
		return dash(strings.Join(emails, ", "))
	}
	return dash(access)
}

func stepsLabel(n int) string {
	if n == 0 {
		return "no limit"
	}
	return strconv.Itoa(n)
}

func budgetLabel(micros int64) string {
	if micros == 0 {
		return "none of its own"
	}
	return policy.FormatMoney(micros)
}

// body is what create and set send: only the fields given.
func (r *agentRun) body() (map[string]any, error) {
	b := map[string]any{}
	for name, v := range map[string]string{"name": r.name, "model": r.model, "description": r.description} {
		if given(r.fs, name) {
			b[name] = v
		}
	}
	if given(r.fs, "instructions") {
		text, err := textOrFile(r.instructions)
		if err != nil {
			return nil, err
		}
		b["instructions"] = text
	}
	if given(r.fs, "tools") {
		b["builtins"] = append([]string{}, r.tools...)
	}
	if given(r.fs, "max-steps") {
		b["max_steps"] = r.maxSteps
	}
	if given(r.fs, "people") {
		b["allowed_emails"] = append([]string{}, r.people...)
		// Naming people only means something when they decide.
		if !given(r.fs, "access") {
			r.access = "people"
		}
	}
	if given(r.fs, "access") || given(r.fs, "people") {
		b["access"] = r.access
	}
	if given(r.fs, "budget") {
		b["budget_micros"] = micros(r.budget)
	}
	return b, nil
}

func (r *agentRun) create(ctx context.Context, a *agents) error {
	if !given(r.fs, "model") {
		return errors.New("--model is required: the model it works with ('keera model list' has them)")
	}
	b, err := r.body()
	if err != nil {
		return err
	}
	b["name"] = r.fs.Arg(0)
	if r.cpu != "" || r.memory != "" {
		// Keera Agents fills in the rest, and the strongest isolation it has.
		b["class"] = machineClass{CPU: r.cpu, Memory: r.memory}
	}
	var e agentEntry
	if err := a.do(ctx, "POST", "/api/v1/agents", b, &e); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, e, nil)
	}
	fmt.Printf("Created %s. Its machine is starting. Give it a task with:\n\n  keera agent ask %s \"...\"\n",
		e.Name, shellQuote(e.Name))
	return nil
}

func (r *agentRun) set(ctx context.Context, a *agents) error {
	if !changesSomething(r.fs) {
		return nothingToChange("agent set")
	}
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := r.body()
	if err != nil {
		return err
	}
	if given(r.fs, "cpu") || given(r.fs, "memory") {
		// The whole class is sent, so the isolation and disk stay as they are.
		c := e.Class
		if given(r.fs, "cpu") {
			c.CPU = r.cpu
		}
		if given(r.fs, "memory") {
			c.Memory = r.memory
		}
		b["class"] = c
	}
	var changed agentEntry
	if err := a.do(ctx, "PATCH", agentPath(e.ID), b, &changed); err != nil {
		return err
	}
	return out(r.asJSON, changed, func(w *table) { printAgents(w, changed) })
}

func (r *agentRun) delete(ctx context.Context, a *agents) error {
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirm("Delete the agent "+e.Name+"?", []string{
			"Its machine, its files and its schedules are deleted, and its key is revoked.",
			"Its tasks stay in 'keera task list'.",
		}, "agent's name", e.Name, "nothing was deleted"); err != nil {
			return err
		}
	}
	if err := a.do(ctx, "DELETE", agentPath(e.ID), nil, nil); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, map[string]string{"deleted": e.ID}, nil)
	}
	fmt.Printf("Deleted %s.\n", e.Name)
	return nil
}

// lifecycle starts, stops, restarts or takes over an agent, as r.verb says.
func (r *agentRun) lifecycle(ctx context.Context, a *agents) error {
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var changed agentEntry
	if err := a.do(ctx, "POST", agentPath(e.ID)+"/"+r.verb, struct{}{}, &changed); err != nil {
		return err
	}
	return out(r.asJSON, changed, func(w *table) { printAgents(w, changed) })
}

// ask gives an agent a task, as the panel's "Give a task" does.
func (r *agentRun) ask(ctx context.Context, a *agents) error {
	if r.wait && r.asJSON {
		return errors.New("--wait prints as the agent works; leave out --json")
	}
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	prompt, err := textOrFile(r.fs.Arg(1))
	if err != nil {
		return err
	}
	var t taskEntry
	if err := a.do(ctx, "POST", agentPath(e.ID)+"/tasks", map[string]string{"prompt": prompt}, &t); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, t, nil)
	}
	if r.wait {
		return followTask(ctx, a, t, os.Stdout)
	}
	fmt.Printf("Gave %s the task %s. Follow it with:\n\n  keera task show %s --follow\n", e.Name, t.ID, t.ID)
	return nil
}

func (r *agentRun) logs(ctx context.Context, a *agents) error {
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return a.copyText(ctx, logsPath(agentPath(e.ID), r.follow), os.Stdout)
}

func (r *agentRun) cost(ctx context.Context, a *agents) error {
	e, err := findAgent(ctx, a, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var c machineCost
	if err := a.do(ctx, "GET", agentPath(e.ID)+"/cost", nil, &c); err != nil {
		return err
	}
	return out(r.asJSON, c, func(w *table) { printMachineCost(w, e.Name, c) })
}

// shellQuote quotes a name for a command we suggest, when it has a space.
func shellQuote(s string) string {
	if strings.ContainsAny(s, " '\"") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
