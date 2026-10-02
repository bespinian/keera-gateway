package control

// What a deployment is missing, in one answer: "why does this not work",
// asked by someone who does not yet know what to suspect.
//
// The checks run here because this is where the facts are. `keera doctor`
// only renders them.
//
// Nothing here writes, so it is safe to run on a live deployment during an
// incident.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Verdict is how a check came out.
type Verdict string

const (
	// VerdictOK is set up and working as far as this can tell.
	VerdictOK Verdict = "ok"
	// VerdictWarn is a deployment that runs, but lacks something it will
	// want: an identity provider, a metrics token.
	VerdictWarn Verdict = "warn"
	// VerdictFail is something that stops requests being served now.
	VerdictFail Verdict = "fail"
)

// Check is one thing a deployment either has or has not.
type Check struct {
	// Area groups the checks: the deployment, the catalogue, the tenancy,
	// what sits in front of a request.
	Area    string  `json:"area"`
	Name    string  `json:"name"`
	Verdict Verdict `json:"verdict"`
	// Detail is what was found, for whoever has to act on it.
	Detail string `json:"detail"`
	// Fix is what to do about it, empty on a check that passed. Every failing
	// check has one.
	Fix string `json:"fix,omitempty"`
}

// Diagnosis is every check, with the totals a caller would otherwise count.
type Diagnosis struct {
	Checks   []Check `json:"checks"`
	Failures int     `json:"failures"`
	Warnings int     `json:"warnings"`
	// Probed says whether the inference plane was actually called. Without a
	// probe, a model that answers looks the same as one that is only declared.
	Probed bool `json:"probed"`
}

func (d *Diagnosis) add(area, name string, verdict Verdict, detail, fix string) {
	d.Checks = append(d.Checks, Check{Area: area, Name: name, Verdict: verdict,
		Detail: detail, Fix: fix})
	switch verdict {
	case VerdictFail:
		d.Failures++
	case VerdictWarn:
		d.Warnings++
	}
}

// diagnostics answers GET /v1/diagnostics.
func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	// An administrator sees their organisation's part. The deployment's part
	// names environment variables and the inference plane, which belong to
	// the operator.
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	// A probe calls one organisation's models, as `keera model check` does,
	// so it is for the same people: that organisation's administrators.
	probe := httpx.Flag(r.URL.Query(), "probe")
	if probe && orgID == "" {
		badRequest(w, "a probe calls one organisation's models: pass org_id, or --org on the "+
			"command line")
		return
	}
	if probe && !p.CanAdminOrg(orgID) {
		s.forbid(w, "a probe calls this organisation's models and puts load on them; only "+
			"its administrators can run one")
		return
	}

	d := &Diagnosis{Probed: probe}
	s.checkDeployment(r.Context(), d, p)
	s.checkCatalogue(r.Context(), d, orgID, p, probe)
	s.checkTenancy(r.Context(), d, orgID, p)
	s.checkHooks(r.Context(), d, orgID, p)
	s.checkSandboxes(r.Context(), d, orgID)
	httpx.WriteJSON(w, http.StatusOK, d)
}

// checkDeployment checks the environment the gateway was started with.
func (s *Server) checkDeployment(ctx context.Context, d *Diagnosis, p *authn.Principal) {
	const area = "Deployment"

	// The deployment's configuration, which a tenant's administrator is not
	// told about.
	if !p.Unrestricted() {
		return
	}

	if s.opts.PublicURL == "" {
		d.add(area, "Public URL", VerdictWarn,
			"not set, so a client configuration falls back to the Host header and a "+
				"refusal cannot link a developer to their limits",
			"set KEERA_PUBLIC_URL to this gateway as a browser reaches it")
	} else {
		d.add(area, "Public URL", VerdictOK, s.opts.PublicURL, "")
	}

	s.checkIdentity(d)

	if s.opts.MetricsToken == "" {
		d.add(area, "Metrics", VerdictWarn,
			"no token, so only the operator key can scrape /metrics - and that key also "+
				"sets every guardrail in the deployment",
			"set KEERA_METRICS_TOKEN to a value of its own")
	} else {
		d.add(area, "Metrics", VerdictOK, "/metrics has a credential of its own", "")
	}

	if err := s.st.Ping(ctx); err != nil {
		d.add(area, "Database", VerdictFail, "Postgres did not answer",
			"check KEERA_DATABASE_URL and that the database is reachable")
	} else {
		d.add(area, "Database", VerdictOK, "Postgres answers", "")
	}
}

// checkIdentity checks how people sign in.
func (s *Server) checkIdentity(d *Diagnosis) {
	const area = "Deployment"
	names := s.opts.Providers.Names()
	if s.opts.Passkeys != nil {
		names = append(names, authn.PasskeyProvider)
	}
	if len(names) > 0 {
		d.add(area, "Identity", VerdictOK, strings.Join(names, ", "), "")
		return
	}
	d.add(area, "Identity", VerdictWarn,
		"none, so the operator key is the only way in and no audit entry can name "+
			"a person",
		"name a provider in KEERA_OIDC_PROVIDERS and set its "+
			"KEERA_OIDC_<NAME>_ISSUER, _CLIENT_ID and _CLIENT_SECRET, or set "+
			"KEERA_PASSKEYS=true; see docs/sso.md")
}

// checkCatalogue checks the models, which is where a deployment that serves
// nothing is usually broken. They are one organisation's.
func (s *Server) checkCatalogue(ctx context.Context, d *Diagnosis, orgID string,
	p *authn.Principal, probe bool,
) {
	if orgID == "" {
		return // an operator looking at every organisation at once
	}
	const area = "Models"
	org := orgFlag(p, orgID)

	models, err := s.st.ListModels(ctx, orgID)
	if err != nil {
		d.add(area, "Catalogue", VerdictFail, "the models could not be read", "")
		return
	}
	var enabled, chat []policy.Model
	for _, m := range models {
		if m.Enabled {
			enabled = append(enabled, m)
			if m.Kind == policy.KindChat {
				chat = append(chat, m)
			}
		}
	}
	switch {
	case len(enabled) == 0:
		d.add(area, "Catalogue", VerdictFail,
			fmt.Sprintf("%d declared, none enabled: every inference request is refused",
				len(models)),
			"add one with 'keera model add <alias> --backend <url>"+org+"', or --provider "+
				"for a hosted one")
	case len(chat) == 0:
		d.add(area, "Catalogue", VerdictWarn,
			fmt.Sprintf("%d enabled, none of them chat: no coding agent can use this "+
				"deployment", len(enabled)),
			"add a chat model with 'keera model add <alias> --backend <url>"+org+"'")
	default:
		d.add(area, "Catalogue", VerdictOK,
			fmt.Sprintf("%d enabled, %d of them chat", len(enabled), len(chat)), "")
	}

	for _, m := range enabled {
		if len(m.Backends) == 0 {
			d.add(area, m.Alias, VerdictFail,
				"enabled with no backend, so a request naming it gets a 503",
				modelFix(m, "--backend <url>", p))
		}
		// A router is told nothing about a destination but its description.
		if m.Description == "" {
			d.add(area, m.Alias, VerdictWarn,
				"no description, so a router choosing between destinations sees a bare "+
					"alias, and so does a client on /v1/models",
				modelFix(m, "--description \"...\"", p))
		}
	}

	if probe {
		s.probeModels(ctx, d, enabled)
	}
}

// modelFix says how to change a model. Only an operator names the
// organisation; anyone else's is their own.
func modelFix(m policy.Model, flags string, p *authn.Principal) string {
	if p.Unrestricted() {
		flags += " --org " + m.OrgID
	}
	return "'keera model set " + m.Alias + " " + flags + "'"
}

// probeModels calls each enabled model. It catches a vLLM parser that does
// not match its model: a 200 with prose instead of a tool call, which leaves
// every coding agent useless.
func (s *Server) probeModels(ctx context.Context, d *Diagnosis, enabled []policy.Model) {
	const area = "Models"
	for _, m := range enabled {
		// Each caller's own Claude plan pays for a subscription model, so the
		// gateway has no credential to probe it with.
		if m.Subscription {
			continue
		}
		live, found := s.reg.Model(m.OrgID, m.Alias)
		if !found {
			continue
		}
		result := s.opts.Gateway.CheckModel(ctx, live)
		switch {
		case !result.Reachable:
			d.add(area, m.Alias+" (probe)", VerdictFail,
				"the backend could not be reached: "+result.Error,
				"check the backend URL and that the inference plane is up")
		case result.ToolCallAsText:
			d.add(area, m.Alias+" (probe)", VerdictFail,
				"the model called a tool and the parser returned it as prose; coding "+
					"agents will not work against this",
				"match vLLM's --tool-call-parser to this model")
		case !result.ToolCalls && m.Kind == policy.KindChat:
			d.add(area, m.Alias+" (probe)", VerdictWarn,
				"answered, but produced no tool call when asked for one",
				"check the tool-call parser; agents need this to work")
		default:
			d.add(area, m.Alias+" (probe)", VerdictOK,
				fmt.Sprintf("answered in %dms, tool calls work", result.TTFTMillis), "")
		}
	}
}

// checkTenancy checks the organisations. Its one trap only appears once a
// second organisation exists.
func (s *Server) checkTenancy(ctx context.Context, d *Diagnosis, orgID string, p *authn.Principal) {
	const area = "Tenancy"

	setup, err := s.st.SetupState(ctx, orgID)
	if err != nil {
		return
	}
	switch {
	case setup.Orgs == 0:
		d.add(area, "Organisations", VerdictFail, "there is none",
			"create one with 'keera org create <name>'")
	case setup.Keys == 0:
		d.add(area, "API keys", VerdictWarn, "no key has been issued, so nothing can call this "+
			"deployment yet",
			"issue one with 'keera key create --team <team> --alias <what for>"+orgFlag(p, orgID)+"'")
	default:
		d.add(area, "API keys", VerdictOK, fmt.Sprintf("%d active", setup.Keys), "")
	}

	if !p.Unrestricted() {
		return
	}
	// The trap: with one organisation no domain is needed. With two, a
	// sign-in matching neither is refused, and the first organisation is the
	// one that usually has no domain.
	orgs, err := s.st.ListOrgs(ctx)
	if err != nil || len(orgs) < 2 {
		return
	}
	var missing []string
	for _, o := range orgs {
		if o.EmailDomain == "" {
			missing = append(missing, o.Name)
		}
	}
	if len(missing) == 0 {
		d.add(area, "Email domains", VerdictOK,
			fmt.Sprintf("all %d organisations have one", len(orgs)), "")
		return
	}
	sort.Strings(missing)
	d.add(area, "Email domains", VerdictFail,
		fmt.Sprintf("%d organisations, and %s ha%s no email domain: a sign-in matching "+
			"none of them is refused rather than placed",
			len(orgs), strings.Join(missing, ", "), plural(len(missing))),
		"set one on each with 'keera org set <org-id> --domain <domain>'")
}

// checkHooks checks the filters and routers: the two things in front of a
// request that can name a model that is not there.
func (s *Server) checkHooks(ctx context.Context, d *Diagnosis, orgID string, p *authn.Principal) {
	if orgID == "" {
		return // an operator looking at every organisation at once
	}
	models, err := s.st.ListModels(ctx, orgID)
	if err != nil {
		return
	}
	enabled := map[string]bool{}
	for _, m := range models {
		if m.Enabled {
			enabled[m.Alias] = true
		}
	}
	org := orgFlag(p, orgID)
	s.checkFilters(ctx, d, orgID, org, enabled)
	s.checkRouters(ctx, d, orgID, org, enabled)
}

func (s *Server) checkFilters(ctx context.Context, d *Diagnosis, orgID, org string, enabled map[string]bool) {
	filters, err := s.st.ListFilters(ctx, orgID)
	if err != nil {
		return
	}
	for _, f := range filters {
		switch {
		case f.UsesModel() && !enabled[f.Model]:
			// A filter fails closed, so this refuses every request it covers.
			d.add("Filters", f.Alias, VerdictFail,
				"runs on '"+f.Model+"', which is not enabled; a filter that cannot run "+
					"refuses every request it covers",
				"point it at an enabled model with 'keera filter set "+f.Alias+
					" --model <alias>"+org+"'")
		case f.Shadow:
			d.add("Filters", f.Alias, VerdictWarn,
				"in shadow: it runs, it costs what it costs, and it enforces nothing",
				"turn it on with 'keera filter set "+f.Alias+" --enforce"+org+"'")
		default:
			d.add("Filters", f.Alias, VerdictOK, string(f.Mode)+", enforcing", "")
		}
	}
}

func (s *Server) checkRouters(ctx context.Context, d *Diagnosis, orgID, org string, enabled map[string]bool) {
	routers, err := s.st.ListRouters(ctx, orgID)
	if err != nil {
		return
	}
	for _, rt := range routers {
		var dead []string
		for _, dest := range rt.Destinations {
			if !enabled[dest] {
				dead = append(dead, dest)
			}
		}
		switch {
		case len(dead) == len(rt.Destinations) && len(dead) > 0:
			d.add("Routers", rt.Alias, VerdictFail,
				"none of its destinations is an enabled model: "+strings.Join(dead, ", "),
				"point it at enabled models with 'keera router set "+rt.Alias+
					" --destinations <a,b>"+org+"'")
		case len(dead) > 0:
			// Not fatal, since the router uses what is left. Still worth
			// saying, because a router's failures are silent.
			d.add("Routers", rt.Alias, VerdictWarn,
				"destinations that are not enabled models: "+strings.Join(dead, ", "),
				"remove them, or enable them")
		case rt.Decides() && !enabled[rt.Model]:
			d.add("Routers", rt.Alias, VerdictFail,
				"reads requests with '"+rt.Model+"', which is not an enabled model",
				"point it at one with 'keera router set "+rt.Alias+" --model <alias>"+org+"'")
		default:
			d.add("Routers", rt.Alias, VerdictOK,
				fmt.Sprintf("%s, %d destinations", rt.Mode, len(rt.Destinations)), "")
		}
	}
}

// checkSandboxes checks the machines.
func (s *Server) checkSandboxes(ctx context.Context, d *Diagnosis, orgID string) {
	const area = "Sandboxes"

	if orgID == "" {
		return // an operator looking at every organisation at once
	}
	classes, err := s.st.ListSandboxClasses(ctx, orgID)
	if err != nil {
		return
	}
	if s.opts.Sandboxes == nil {
		// Not a warning: every new organisation gets the template's classes,
		// driver or not, so they are there once one is set.
		if len(classes) > 0 {
			d.add(area, "Driver", VerdictOK,
				fmt.Sprintf("sandboxes are off; set KEERA_SANDBOX_DRIVER to lend "+
					"out the %d classes", len(classes)),
				"")
		}
		return
	}
	d.add(area, "Driver", VerdictOK, fmt.Sprintf("%d classes", len(classes)), "")
}

// plural completes "has" or "have" after "ha".
// orgFlag is the --org a suggested command needs. Only an operator names the
// organisation; anyone else's is their own.
func orgFlag(p *authn.Principal, orgID string) string {
	if p.Unrestricted() {
		return " --org " + orgID
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return "s"
	}
	return "ve"
}
