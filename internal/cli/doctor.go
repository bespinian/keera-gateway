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
	"strings"

	"github.com/bespinian/keera-gateway/internal/control"
)

func doctorCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	probe := fs.Bool("probe", false,
		"put a real request through every enabled model; costs a generation each")
	org := fs.String("org", "", "organisation to check (defaults to the only one, if there is only one)")
	asJSON := fs.Bool("json", false, "print raw JSON")
	fs.Usage = func() { _ = printHelp(fs, "doctor", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "doctor", want)
	}
	if err := parse(fs, args); err != nil {
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
		return fmt.Errorf("%d check%s failed", d.Failures, pluralS(d.Failures))
	}
	return nil
}

func printDiagnosis(base string, d control.Diagnosis) {
	fmt.Printf("%s\n\n", style.head(base))

	// Details and fixes are wrapped to the column after two spaces, the
	// verdict, a space, the padded name and a space.
	const nameWidth = 18
	const gutter = 2 + 4 + 1 + nameWidth + 1

	area := ""
	for _, check := range d.Checks {
		if check.Area != area {
			area = check.Area
			fmt.Printf("%s\n", style.head(area))
		}
		fmt.Printf("  %s %-*s %s\n", mark(check.Verdict), nameWidth, check.Name,
			wrapAt(check.Detail, gutter))
		if check.Fix != "" {
			fmt.Printf("%s%s\n", strings.Repeat(" ", gutter),
				style.cmd(wrapAt("→ "+check.Fix, gutter)))
		}
	}

	fmt.Println()
	switch {
	case d.Failures > 0:
		fmt.Printf("%s, %d worth fixing.\n",
			style.bad(fmt.Sprintf("%d failing", d.Failures)), d.Warnings)
	case d.Warnings > 0:
		fmt.Printf("Nothing is broken. %s worth fixing.\n",
			style.warn(fmt.Sprintf("%d thing%s", d.Warnings, pluralS(d.Warnings))))
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

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
