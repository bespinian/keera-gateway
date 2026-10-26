package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Claude Code signed in to a Claude plan, for `keera connect claude-code
// --subscription`. See docs/subscriptions.md.

var errSubscriptionsOff = errors.New("this deployment does not take Claude subscriptions; " +
	"run 'keera connect claude-code' without --subscription to use a Keera key instead")

func saidKey(key createdKey, replaced *store.KeySummary) {
	switch {
	case replaced != nil && replaced.Pending:
		fmt.Fprintf(os.Stderr, "Activated your key %s (%s…), which is now this machine's.\n",
			key.ID, key.Prefix)
	case replaced != nil && replaced.Name == key.Name:
		fmt.Fprintf(os.Stderr, "Replaced this machine's key %s with %s (%s…).\n", replaced.ID, key.ID, key.Prefix)
	case replaced != nil:
		fmt.Fprintf(os.Stderr, "Replaced your key %s (%s) with %s (%s…), which is now this machine's.\n",
			replaced.ID, replaced.Name, key.ID, key.Prefix)
	default:
		fmt.Fprintf(os.Stderr, "Issued this machine the key %s (%s…).\n", key.ID, key.Prefix)
	}
}

// subscriptionEnv returns settings for the subscription models, with the main
// one's alias.
func subscriptionEnv(models []policy.Model, named string) (map[string]string, string, error) {
	subs := subscriptionModels(models)
	if len(subs) == 0 {
		return nil, "", errors.New("this organisation has no subscription model, so a Claude " +
			"plan cannot pay for anything here; an administrator adds one with: keera model " +
			"add claude-opus --provider anthropic --backend-model claude-opus-5-5 --subscription")
	}
	env, alias, ok := claudeModelEnv(subs, named)
	if !ok {
		return nil, "", fmt.Errorf("no enabled subscription model %s; this organisation "+
			"has: %s", named, strings.Join(aliasesOf(subs), ", "))
	}
	// Claude Code turns tool search off for any address that is not
	// Anthropic's. The gateway forwards a subscription request unchanged, so
	// tool search works through it.
	env["ENABLE_TOOL_SEARCH"] = "true"
	return env, alias, nil
}

// claudeModelEnv points Claude Code at the model named, or else the first. It
// reports false when no model has that name. See connect.ClaudeEnv.
func claudeModelEnv(models []policy.Model, named string) (map[string]string, string, bool) {
	main := models[0]
	if named != "" {
		found, ok := findModel(models, named)
		if !ok {
			return nil, "", false
		}
		main = found
	}
	return connect.ClaudeEnv(connectList(models), connectModel(main)),
		main.Alias, true
}

func subscriptionModels(models []policy.Model) []policy.Model {
	var subs []policy.Model
	for _, m := range models {
		if m.Enabled && m.Subscription && m.Kind == policy.KindChat {
			subs = append(subs, m)
		}
	}
	return subs
}

// setModelOverrides keeps the entries for other models.
func setModelOverrides(settings map[string]any, overrides map[string]string) {
	block, _ := settings["modelOverrides"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	for id, alias := range overrides {
		block[id] = alias
	}
	if len(block) > 0 {
		settings["modelOverrides"] = block
	}
}

// keyStep is planned before it is taken, because replacing a key revokes the
// old one at once.
type keyStep struct {
	replaces *store.KeySummary
	issue    func(ctx context.Context, c *client) (createdKey, error)
}

// planMachineKey tries, in order: rotate this machine's key; activate a
// pending key of the caller's; take over a subscription key issued to a member
// that no machine holds yet; issue a new key as an administrator. A member
// with none of these is sent to an administrator.
func planMachineKey(ctx context.Context, c *client, orgID string, me identity, project string,
	kind policy.KeyKind,
) (keyStep, error) {
	member := me.Role == string(authn.RoleMember)
	if member && project != "" {
		return keyStep{}, errors.New("--project is for administrators; your key keeps " +
			"the project an administrator issued it in")
	}
	machine, err := machineID()
	if err != nil {
		return keyStep{}, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "this machine"
	}
	name := machineKeyName(host, machine)
	inProject := ""
	if !member {
		if inProject, err = projectID(ctx, c, orgID, project); err != nil {
			return keyStep{}, err
		}
	}
	keys, err := list[store.KeySummary](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return keyStep{}, err
	}
	activate := func(k *store.KeySummary, name string) keyStep {
		return keyStep{replaces: k, issue: func(ctx context.Context, c *client) (createdKey, error) {
			return activateKey(ctx, c, k.ID, name)
		}}
	}
	rotate := func(k *store.KeySummary, name string) keyStep {
		return keyStep{replaces: k, issue: func(ctx context.Context, c *client) (createdKey, error) {
			rotated, err := rotateKey(ctx, c, k.ID, name, nil)
			return rotated.createdKey, err
		}}
	}
	var pending, unclaimed *store.KeySummary
	for i, k := range keys {
		if k.UserID != me.UserID || k.Kind != kind || k.RevokedAt != nil {
			continue
		}
		inOther := project != "" && inProject != k.ProjectID
		switch {
		case k.Name == name && inOther:
			// A rotation keeps the project, so --project would be silently ignored.
			return keyStep{}, fmt.Errorf("this machine already has the key %s, in "+
				"another project, and replacing it keeps that project; to move it, revoke it "+
				"first with: keera key revoke %s, then run this again", k.ID, k.ID)
		case k.Name == name && k.Pending:
			return activate(&keys[i], ""), nil
		case k.Name == name:
			return rotate(&keys[i], ""), nil
		case k.Pending && pending == nil && !inOther:
			pending = &keys[i]
		case !k.Pending && unclaimed == nil && kind == policy.KeySubscription &&
			!isMachineKeyName(k.Name):
			unclaimed = &keys[i]
		}
	}
	switch {
	case pending != nil:
		return activate(pending, name), nil
	case member && unclaimed != nil:
		return rotate(unclaimed, name), nil
	case member:
		which := ""
		if kind == policy.KeySubscription {
			which = " --subscription"
		}
		return keyStep{}, fmt.Errorf("you have no key to activate for this machine, "+
			"and only an administrator can issue one; ask one to run: keera key create%s "+
			"--pending --user %s --name \"%s's Claude Code\", then run this again",
			which, me.Email, strings.Split(me.Email, "@")[0])
	}
	return keyStep{issue: func(ctx context.Context, c *client) (createdKey, error) {
		var created createdKey
		err := c.do(ctx, "POST", "/v1/keys", map[string]any{
			"org_id": orgID, "project_id": inProject, "user_id": me.UserID, "name": name,
			"kind": string(kind), "expires_in_seconds": int64(defaultKeyLife / time.Second),
		}, &created)
		return created, err
	}}, nil
}

// machineKeyName includes the id to tell apart machines that share a
// hostname, such as copies of one VM image.
func machineKeyName(host, machine string) string {
	return "claude-code on " + host + " (" + machine + ")"
}

var machineKeyNameRE = regexp.MustCompile(`^claude-code on .+ \([0-9a-f]{8}\)$`)

// isMachineKeyName reports whether a key already belongs to a machine, which
// taking it over would break.
func isMachineKeyName(name string) bool {
	return machineKeyNameRE.MatchString(name)
}

// machineID is random, made on first use, and kept next to the sign-in.
func machineID() (string, error) {
	creds, err := credentialsPath()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(creds), "machine-id")
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 4)
	_, _ = rand.Read(buf) // never fails; see crypto/rand.Read
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("keeping this machine's id: %w", err)
	}
	return id, nil
}

// claudeConfigDir honours CLAUDE_CONFIG_DIR, as Claude Code does.
func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding Claude Code's settings: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func managedSettingsDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	default:
		return "/etc/claude-code"
	}
}

// managedBaseURL is the address Claude Code's managed settings set, or "".
// serverCache, the copy from the claude.ai admin console, wins; then the files
// in systemDir, a later drop-in over an earlier one. MDM profiles and the
// Windows registry are not read.
func managedBaseURL(serverCache, systemDir string) string {
	if u := settingsBaseURL(serverCache); u != "" {
		return u
	}
	files := []string{filepath.Join(systemDir, "managed-settings.json")}
	dropIns, _ := filepath.Glob(filepath.Join(systemDir, "managed-settings.d", "*.json"))
	found := ""
	for _, f := range append(files, dropIns...) {
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue
		}
		if u := settingsBaseURL(f); u != "" {
			found = u
		}
	}
	return found
}

// settingsBaseURL reads ANTHROPIC_BASE_URL from a settings file's 'env' block.
func settingsBaseURL(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	u, _ := doc.Env["ANTHROPIC_BASE_URL"].(string)
	return strings.TrimSpace(u)
}

// readSettings returns an empty map when there is no file yet.
func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	settings := map[string]any{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, so it was left alone; fix it first: %w", path, err)
	}
	for _, name := range []string{"env", "modelOverrides"} {
		if v, ok := settings[name]; ok {
			if _, isObject := v.(map[string]any); !isObject {
				return nil, fmt.Errorf("the '%s' setting in %s is not an object, so it was left alone",
					name, path)
			}
		}
	}
	return settings, nil
}

// setClaudeEnv writes the 'env' block and warns about what else in the file
// would stop it working. An empty value removes a setting. A subscription key
// goes in ANTHROPIC_CUSTOM_HEADERS; without one, an earlier one is removed,
// because the gateway reads that header first.
func setClaudeEnv(settings map[string]any, env map[string]string, subscriptionKey string) []string {
	block, _ := settings["env"].(map[string]any)
	if block == nil {
		block = map[string]any{}
		settings["env"] = block
	}
	for name, value := range env {
		if value == "" {
			delete(block, name)
		} else {
			block[name] = value
		}
	}
	var headers []string
	if old, ok := block["ANTHROPIC_CUSTOM_HEADERS"].(string); ok {
		for line := range strings.SplitSeq(old, "\n") {
			name, _, _ := strings.Cut(line, ":")
			if strings.TrimSpace(line) != "" && !strings.EqualFold(strings.TrimSpace(name), gateway.KeyHeader) {
				headers = append(headers, line)
			}
		}
	}
	if subscriptionKey != "" {
		headers = append(headers, gateway.KeyHeader+": "+subscriptionKey)
	}
	if len(headers) == 0 {
		delete(block, "ANTHROPIC_CUSTOM_HEADERS")
	} else {
		block["ANTHROPIC_CUSTOM_HEADERS"] = strings.Join(headers, "\n")
	}

	if subscriptionKey == "" {
		if _, set := settings["apiKeyHelper"]; set {
			return []string{"Remove the 'apiKeyHelper' setting: while it is set, Claude Code " +
				"sends its key instead of the one written here."}
		}
		return nil
	}
	// Any of these makes Claude Code send it instead of the Claude sign-in.
	var warnings []string
	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if _, set := block[name]; set {
			warnings = append(warnings, "Remove "+name+" from the 'env' block: while it is set, "+
				"Claude Code sends it instead of your Claude sign-in.")
		}
		if os.Getenv(name) != "" {
			warnings = append(warnings, "Unset "+name+" in your shell: while it is set, Claude "+
				"Code sends it instead of your Claude sign-in.")
		}
	}
	if _, set := settings["apiKeyHelper"]; set {
		warnings = append(warnings, "Remove the 'apiKeyHelper' setting: while it is set, "+
			"Claude Code sends its key instead of your Claude sign-in.")
	}
	return warnings
}
