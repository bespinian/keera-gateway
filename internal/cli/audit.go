package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// auditMax is the most entries the control API answers in one page.
const auditMax = 5000

// auditCmd is the panel's Audit screen in a terminal: who changed what,
// newest first.
func auditCmd(ctx context.Context, args []string) error {
	r := newCmdRun("audit", args)
	fs := r.fs
	action := fs.String("action", "", "only this action, such as key.revoke")
	actor := fs.String("actor", "", "only what this person did, by email")
	since := new(time.Duration)
	// The same window as the panel's.
	durationVar(fs, since, "since", 30*24*time.Hour, sinceUsage)
	limit := fs.Int("limit", reportLimit, fmt.Sprintf("how many to print, up to %d", auditMax))
	if done, err := r.parse(); done {
		return err
	}
	if *limit < 1 || *limit > auditMax {
		return fmt.Errorf("--limit takes 1 to %d", auditMax)
	}
	q, err := reportQuery(ctx, r.c, r.org, nil, *since, map[string]string{
		"action": *action, "actor": *actor, "limit": strconv.Itoa(*limit),
	})
	if err != nil {
		return err
	}
	return doOut(ctx, r.c, "GET", "/v1/audit?"+q.Encode(), nil, r.asJSON, func(w *table, res auditResponse) {
		printAudit(w, res, *since, *limit)
		// A typo in --action matches nothing, so say which ones there are.
		if len(res.Data) == 0 && *action != "" && len(res.Filters.Actions) > 0 {
			_, _ = fmt.Fprintf(w, "Actions in the log: %s\n", strings.Join(res.Filters.Actions, ", "))
		}
	})
}

type auditResponse struct {
	Data    []store.AuditEntry `json:"data"`
	Filters store.AuditFacets  `json:"filters"`
}

func printAudit(w *table, res auditResponse, since time.Duration, limit int) {
	if len(res.Data) == 0 {
		printNone(w, "changes in the last "+shortDuration(since), "")
		return
	}
	w.header("WHEN\tWHO\tACTION\tTARGET\tDETAIL")
	for _, e := range res.Data {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Local().Format("2006-01-02 15:04:05"), dash(e.Actor), e.Action,
			dash(strings.TrimSpace(e.TargetType+" "+e.TargetID)), auditDetail(e.Detail))
	}
	if len(res.Data) == limit {
		_, _ = fmt.Fprintf(w, "\nShowing the newest %d. There may be older ones: raise --limit or shorten --since.\n",
			limit)
	}
}

// auditDetail keeps the detail on its row. --json has it whole.
func auditDetail(raw []byte) string {
	s := string(raw)
	if s == "null" || s == "{}" {
		return "-"
	}
	return oneLine(s)
}
