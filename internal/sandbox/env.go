package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// environment puts the manager's values over the caller's Env, so a caller
// cannot redirect a sandbox's gateway address or key.
func (m *Manager) environment(ctx context.Context, req CreateRequest, row store.Sandbox,
	secret string, expires time.Time,
) map[string]string {
	env := map[string]string{}
	maps.Copy(env, req.Env)

	env["KEERA_SANDBOX_ID"] = row.ID
	env["KEERA_SANDBOX_NAME"] = row.Name
	env["KEERA_SANDBOX_CLASS"] = row.Class
	env["KEERA_SANDBOX_PURPOSE"] = string(row.Purpose)
	env["KEERA_SANDBOX_EXPIRES"] = expires.Format(time.RFC3339)
	env["KEERA_API_KEY"] = secret

	if len(req.AuthorizedKeys) > 0 {
		env["KEERA_AUTHORIZED_KEYS"] = strings.Join(req.AuthorizedKeys, "\n")
	}
	// Commits are by the person, not by "keera".
	if req.Owner != "" {
		env["KEERA_GIT_AUTHOR_EMAIL"] = req.Owner
		env["KEERA_GIT_AUTHOR_NAME"] = req.Owner
	}

	// An agent sandbox names its own session (docs/sessions.md), for Claude
	// Code and for Pi below.
	if row.Purpose == policy.PurposeAgent {
		env["KEERA_SESSION"] = row.ID
		env["ANTHROPIC_CUSTOM_HEADERS"] = httpx.SessionHeader + ": " + row.ID
	}

	m.inferenceEnv(env)
	m.agentEnv(ctx, req, env)
	// Passed once at start and never stored.
	if req.Task != "" {
		env["KEERA_TASK"] = req.Task
	}
	return env
}

// inferenceEnv points the sandbox's clients at this gateway.
func (m *Manager) inferenceEnv(env map[string]string) {
	base := m.opts.PublicURL
	if base == "" {
		return
	}
	inference := base + httpx.InferencePrefix
	env["KEERA_BASE_URL"] = inference
	// OpenAI and Anthropic clients disagree on where the version goes.
	env["OPENAI_BASE_URL"] = inference + "/v1"
	env["ANTHROPIC_BASE_URL"] = inference
	// The key is only in KEERA_API_KEY, never under a vendor's name: Pi would
	// arm a built-in provider from it and could send the key and prompts to
	// that vendor (docs/sandboxes.md).
}

// agentEnv configures Pi with every model the key may reach, starting on the
// first.
func (m *Manager) agentEnv(ctx context.Context, req CreateRequest, env map[string]string) {
	models := m.reachableModels(ctx, req)
	if len(models) == 0 && env["KEERA_BASE_URL"] != "" {
		m.log.Warn("sandbox: this key can reach no chat model, so the sandbox is created "+
			"with no agent configuration; check the organisation's and the project's "+
			"allowed models", "org", req.OrgID, "project", req.ProjectID)
	}
	if cfg := piConfig(env["KEERA_BASE_URL"], models, env["KEERA_SESSION"]); cfg != "" {
		env["KEERA_PI_CONFIG"] = cfg
		// Pi prefers an armed built-in provider, so an image that exports a
		// vendor variable would send the key there. Only in a sandbox: `keera
		// connect` must not override a laptop's default.
		env["KEERA_PI_SETTINGS"] = piSettings(models[0].Alias)
	}
	if len(models) > 0 {
		// For the agent runner's --model.
		env["KEERA_MODEL"] = models[0].Alias
		env["ANTHROPIC_MODEL"] = models[0].Alias
	}
}

// piSettings makes this gateway Pi's default provider.
func piSettings(alias string) string {
	raw, err := json.Marshal(map[string]any{
		"defaultProvider": piProvider,
		"defaultModel":    alias,
		// No install ping from inside a customer's cluster.
		"enableInstallTelemetry": false,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

// piProvider must match the connect catalogue's Pi template. A test checks.
const piProvider = "keera"

// piConfig renders Pi's models.json from the `keera connect pi` template, so
// /model offers exactly what the guardrail allows. A session id makes an
// agent's task one session. It is empty with no base URL or no models: no
// configuration is better than a wrong one.
func piConfig(base string, models []connect.Model, session string) string {
	pi, ok := connect.Find(connect.Clients(), "pi")
	if base == "" || len(models) == 0 || !ok {
		return ""
	}
	raw := pi.Render(base, models)
	if session == "" {
		return raw
	}
	var file struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if json.Unmarshal([]byte(raw), &file) != nil || file.Providers[piProvider] == nil {
		return ""
	}
	provider := file.Providers[piProvider]
	headers, _ := provider["headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{}
	}
	headers[httpx.SessionHeader] = session
	provider["headers"] = headers
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// reachableModels is every enabled chat model and router the key may use,
// models first, so the sandbox offers only models that will answer.
func (m *Manager) reachableModels(ctx context.Context, req CreateRequest) []connect.Model {
	var failed error
	limits := func(scope policy.ScopeType, id string) *policy.Limits {
		if id == "" {
			return nil
		}
		lim, err := m.st.GetGuardrail(ctx, scope, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				failed = err
			}
			return nil
		}
		return &lim
	}
	// mintKey issues a standard key, which cannot reach subscription models.
	resolved := policy.Resolve(
		policy.Key{OrgID: req.OrgID, ProjectID: req.ProjectID, UserID: req.UserID, Kind: policy.KeyStandard},
		limits(policy.ScopeOrg, req.OrgID),
		limits(policy.ScopeProject, req.ProjectID),
		nil,
	)
	// Without the guardrails the list could offer forbidden models.
	if failed != nil {
		m.log.Warn("sandbox: reading the guardrails for the agent configuration failed",
			"error", failed)
		return nil
	}

	catalogue, err := m.st.ListModels(ctx, req.OrgID)
	if err != nil {
		m.log.Warn("sandbox: reading the catalogue for the agent configuration failed",
			"error", err)
		return nil
	}
	routers, err := m.st.ListRouters(ctx, req.OrgID)
	if err != nil {
		m.log.Warn("sandbox: reading the routers for the agent configuration failed",
			"error", err)
		return nil
	}
	return chatModelsFor(resolved, catalogue, routers)
}

func chatModelsFor(resolved *policy.Resolved, catalogue []policy.Model,
	routers []policy.Router,
) []connect.Model {
	out := make([]connect.Model, 0, len(catalogue)+len(routers))
	for _, mod := range catalogue {
		if !mod.Enabled || mod.Kind != policy.KindChat || !resolved.MayUse(mod) {
			continue
		}
		out = append(out, connect.Model{
			Alias: mod.Alias, Name: mod.DisplayName,
			MaxContext: mod.MaxContext, BackendModel: mod.BackendModel, Provider: mod.Provider,
		})
	}
	// A router is named and allowed like a model.
	for _, p := range routers {
		if resolved.MayCallRouter(p.Alias) {
			out = append(out, connect.Model{Alias: p.Alias, Name: p.DisplayName})
		}
	}
	return out
}
