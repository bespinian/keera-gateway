package cli

// `keera doctor` - one command for "why does this not work".
//
// The checks and their verdicts come from the control plane (see
// internal/control/diagnostics.go). This renders them and turns a failure
// into an exit code, so a pipeline can run it too.

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/bespinian/keera-gateway/internal/control"
)

func doctorCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	probe := fs.Bool("probe", false,
		"put a real request through every enabled model; costs a generation each")
	org := fs.String("org", "", orgUsage)
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "doctor", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "doctor", want)
	}
	if err := parseCmd(fs, "doctor", args); err != nil {
		return err
	}

	q := url.Values{}
	if *probe {
		q.Set("probe", "1")
	}
	// An organisation is optional: without one, the checks that need it are
	// skipped.
	if *org != "" {
		q.Set("org_id", *org)
	} else if only, err := theOnlyOrg(ctx, c, ""); err == nil && only != "" {
		q.Set("org_id", only)
	}

	path := "/v1/diagnostics"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var d control.Diagnosis
	if err := c.do(ctx, "GET", path, nil, &d); err != nil {
		return err
	}
	if *asJSON {
		return out(true, d, nil)
	}
	printDiagnosis(c.base, d)

	// A failed check fails the command, so an install script can end with it.
	// Warnings do not: a deployment with warnings still works.
	if d.Failures > 0 {
		return fmt.Errorf("%s failed", plural(d.Failures, "check"))
	}
	return nil
}

func printDiagnosis(base string, d control.Diagnosis) {
	fmt.Printf("%s\n\n", style.head(base))

	// Details and fixes are wrapped to fit after the verdict and the longest
	// name. Their later lines start with two empty cells, so they stay in the
	// detail column.
	nameWidth := 0
	for _, check := range d.Checks {
		nameWidth = max(nameWidth, len([]rune(check.Name)))
	}
	gutter := 2 + 4 + tablePad + nameWidth + tablePad
	cells := func(text string, paint func(string) string) string {
		lines := strings.Split(wrapAt(text, gutter, gutter, proseWidth), "\n")
		for i, line := range lines {
			lines[i] = paint(strings.TrimLeft(line, " "))
		}
		return strings.Join(lines, "\n\t\t")
	}
	plain := func(s string) string { return s }

	w := newTable(os.Stdout)
	area := ""
	for _, check := range d.Checks {
		if check.Area != area {
			area = check.Area
			_, _ = fmt.Fprintln(w, style.head(area))
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", mark(check.Verdict), check.Name,
			cells(check.Detail, plain))
		if check.Fix != "" {
			_, _ = fmt.Fprintf(w, "\t\t%s\n", cells("→ "+check.Fix, style.cmd))
		}
	}
	_ = w.Flush()

	fmt.Println()
	switch {
	case d.Failures > 0:
		fmt.Printf("%s, %d worth fixing.\n",
			style.bad(fmt.Sprintf("%d failing", d.Failures)), d.Warnings)
	case d.Warnings > 0:
		fmt.Printf("Nothing is broken. %s worth fixing.\n",
			style.warn(plural(d.Warnings, "thing")))
	default:
		fmt.Println(style.ok("Everything checks out."))
	}
	if !d.Probed {
		// Said every time, so a clean report is not read as proof that the
		// models answer.
		fmt.Println("The inference plane was not called: 'keera doctor --probe' asks each " +
			"model to answer.")
	}
}

// mark is a verdict as a word, painted when there is colour. The padding is
// outside the paint, so the columns are the same either way.
func mark(v control.Verdict) string {
	switch v {
	case control.VerdictFail:
		return style.bad("FAIL")
	case control.VerdictWarn:
		return style.warn("warn")
	default:
		return "  " + style.ok("ok")
	}
}
