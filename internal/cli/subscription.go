package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Claude Code signed in to a Claude plan.
//
// `keera connect claude-code --subscription` gives this machine its own
// subscription key and writes it into Claude Code's settings, next to the
// gateway's address. The person never sees or copies the key. Running it again
// replaces the machine's key instead of adding another. An administrator gets
// a new key; a member takes over one an administrator issued them. See
// docs/subscriptions.md.

// subscriptionSetup is what one `connect --subscription` was asked for.
type subscriptionSetup struct {
	org, project, model string
	gatewayURL          string
	asJSON              bool
}

// connectSubscription sets Claude Code on this machine up for a Claude plan.
func connectSubscription(ctx context.Context, c *client, in subscriptionSetup) error {
	me, err := whoami(ctx, c)
	if err != nil {
		return err
	}
	if me.UserID == "" {
		// The operator key wins over a sign-in, so signing in alone is not enough.
		if c.key != "" {
			return errors.New("a subscription key belongs to a person, and the operator key is " +
				"nobody; unset KEERA_OPERATOR_KEY to run this as yourself, and sign in with " +
				"'keera login' if you have not")
		}
		return errors.New("a subscription key belongs to a person, and the operator key is " +
			"nobody; sign in first with: keera login")
	}
	orgID, err := resolveOrg(ctx, c, in.org)
	if err != nil {
		return err
	}
	models, err := catalogue(ctx, c, orgID)
	if err != nil {
		return err
	}
	env, alias, err := subscriptionEnv(models, in.model)
	if err != nil {
		return err
	}
	dir, err := claudeConfigDir()
	if err != nil {
		return err
	}
	// Claude Code stops fetching the organisation's settings from Anthropic when
	// the user's own settings name an address. So when managed settings set it
	// already, it is left to them, and an earlier copy is taken out.
	managed := managedBaseURL(filepath.Join(dir, "remote-settings.json"), managedSettingsDir())
	if managed == "" {
		env["ANTHROPIC_BASE_URL"] = in.gatewayURL
	} else {
		env["ANTHROPIC_BASE_URL"] = ""
	}

	// The settings are read before a key is issued, so a file that cannot be
	// changed leaves no key behind that nothing holds.
	path := filepath.Join(dir, "settings.json")
	settings, err := readSettings(path)
	if err != nil {
		return err
	}

	key, replaced, err := machineKey(ctx, c, orgID, me, in.project)
	if err != nil {
		return err
	}
	replacedID := ""
	if replaced != nil {
		replacedID = replaced.ID
	}
	warnings := setClaudeEnv(settings, env, key.Key)
	setModelOverrides(settings, claudeModelOverrides(models))
	if managed != "" && strings.TrimRight(managed, "/") != strings.TrimRight(in.gatewayURL, "/") {
		warnings = append(warnings, "Your organisation's managed settings send Claude Code to "+
			managed+", not to "+in.gatewayURL+". The key works only if both are this gateway.")
	}
	if err := writeSettings(path, settings); err != nil {
		return fmt.Errorf("the key %s was issued, but %w; run this again to replace it", key.ID, err)
	}

	if in.asJSON {
		return out(true, map[string]any{
			"settings": path, "key_id": key.ID, "replaced": replacedID, "model": alias,
			"url": in.gatewayURL, "url_managed": managed != "", "warnings": warnings,
		}, nil)
	}
	fmt.Fprintf(os.Stderr, "%s · model %s · %s\n\n", styleErr.head("Claude Code with your Claude plan"),
		alias, in.gatewayURL)
	switch {
	case replaced != nil && replaced.Name == key.Name:
		fmt.Fprintf(os.Stderr, "Replaced this machine's key %s with %s (%s…).\n", replaced.ID, key.ID, key.Prefix)
	case replaced != nil:
		fmt.Fprintf(os.Stderr, "Replaced your key %s (%s) with %s (%s…), which is now this machine's.\n",
			replaced.ID, replaced.Name, key.ID, key.Prefix)
	default:
		fmt.Fprintf(os.Stderr, "Issued this machine the key %s (%s…).\n", key.ID, key.Prefix)
	}
	if managed != "" {
		fmt.Fprintf(os.Stderr, "Wrote the key and the models into %s. The gateway's address "+
			"comes from your organisation's managed settings.\n", path)
	} else {
		fmt.Fprintf(os.Stderr, "Wrote the gateway, the key and the models into %s.\n", path)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "\n%s\n", styleErr.warn(wrapAt(w, 0, 0, 76)))
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", wrapAt("Run 'claude' in your project. If it is not signed in "+
		"to your Claude plan yet, run '/login' there. '/status' shows the gateway.", 0, 0, 76))
	return nil
}

// subscriptionEnv picks the models Claude Code is pointed at, and returns them
// as settings, with the main one's alias.
//
// Claude Code asks for an Opus, a Sonnet or a Haiku model by Anthropic's own
// names, which the gateway does not know. So each of the three is mapped to
// the subscription model of that family, or to the main one when there is none.
func subscriptionEnv(models []policy.Model, named string) (map[string]string, string, error) {
	subs := subscriptionModels(models)
	if len(subs) == 0 {
		return nil, "", errors.New("this organisation has no subscription model, so a Claude " +
			"plan cannot pay for anything here; an administrator adds one with: keera model " +
			"add claude-opus --provider anthropic --backend-model claude-opus-5-5 --subscription")
	}
	main := subs[0]
	if named != "" {
		found, ok := findModel(subs, named)
		if !ok {
			return nil, "", fmt.Errorf("no enabled subscription model %s; this organisation "+
				"has: %s", named, strings.Join(aliasesOf(subs), ", "))
		}
		main = found
	}
	env := map[string]string{
		"ANTHROPIC_MODEL": claudeCodeName(main),
		// Claude Code turns tool search off for any address that is not
		// Anthropic's, and then sends every tool of every MCP server with each
		// request. The gateway forwards a subscription request as it is, so
		// tool search works through it.
		"ENABLE_TOOL_SEARCH": "true",
	}
	for _, family := range []string{"opus", "sonnet", "haiku"} {
		name := claudeCodeName(main)
		for _, m := range subs {
			if strings.Contains(m.BackendModel, family) {
				name = claudeCodeName(m)
				break
			}
		}
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(family)+"_MODEL"] = name
	}
	return env, main.Alias, nil
}

// subscriptionModels is the models a subscription key can reach.
func subscriptionModels(models []policy.Model) []policy.Model {
	var subs []policy.Model
	for _, m := range models {
		if m.Enabled && m.Subscription && m.Kind == policy.KindChat {
			subs = append(subs, m)
		}
	}
	return subs
}

// claudeCodeName is the name Claude Code is given for a model. For a model
// Claude Code does not know, it assumes a 200K context. The [1m] suffix tells
// it the window is 1M, and Claude Code takes the suffix off before it sends the
// request.
func claudeCodeName(m policy.Model) string {
	if m.MaxContext >= 1_000_000 {
		return m.Alias + "[1m]"
	}
	return m.Alias
}

// claudeModelOverrides tells Claude Code which Claude model each alias is.
// Without it, Claude Code does not know the gateway's aliases, and guesses
// what they support: Haiku would be sent adaptive thinking it then refuses.
// Claude Code knows a model by Anthropic's id, and a dated model by the dated
// one, so both are written. The first model by alias wins an id two share.
func claudeModelOverrides(models []policy.Model) map[string]string {
	anthropic, _ := catalog.ProviderByName("anthropic")
	overrides := map[string]string{}
	for _, m := range subscriptionModels(models) {
		ids := []string{m.BackendModel}
		if known, found := anthropic.Model(m.BackendModel); found {
			ids = append(ids, known.ID, known.Snapshot)
		}
		for _, id := range ids {
			if _, taken := overrides[id]; !taken && id != "" {
				overrides[id] = m.Alias
			}
		}
	}
	return overrides
}

// setModelOverrides writes the overrides into the 'modelOverrides' setting,
// keeping the entries for other models.
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

// machineKey replaces this machine's subscription key, or gets it its first one.
// It returns the new key and the one it replaced, if any.
//
// Only an administrator issues keys. So a member's first run takes over a
// subscription key an administrator issued them that no machine holds yet,
// and rotates it to get a secret to write down.
func machineKey(ctx context.Context, c *client, orgID string, me identity, project string,
) (createdKey, *store.KeySummary, error) {
	member := me.Role == string(authn.RoleMember)
	if member && project != "" {
		return createdKey{}, nil, errors.New("--project is for administrators; your key keeps " +
			"the project an administrator issued it in")
	}
	machine, err := machineID()
	if err != nil {
		return createdKey{}, nil, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "this machine"
	}
	name := machineKeyName(host, machine)
	inProject := ""
	if !member {
		if inProject, err = projectID(ctx, c, orgID, project); err != nil {
			return createdKey{}, nil, err
		}
	}
	keys, err := list[store.KeySummary](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return createdKey{}, nil, err
	}
	var unclaimed *store.KeySummary
	for i, k := range keys {
		if k.UserID != me.UserID || k.Kind != policy.KeySubscription || k.RevokedAt != nil {
			continue
		}
		if k.Name == name {
			// A rotation keeps the key's project, so a different --project would be
			// ignored without a word.
			if project != "" && inProject != k.ProjectID {
				return createdKey{}, nil, fmt.Errorf("this machine already has the key %s, in "+
					"another project, and replacing it keeps that project; to move it, revoke it "+
					"first with: keera key revoke %s, then run this again", k.ID, k.ID)
			}
			rotated, err := rotateFor(ctx, c, k.ID, "")
			return rotated, &keys[i], err
		}
		if unclaimed == nil && !isMachineKeyName(k.Name) {
			unclaimed = &keys[i]
		}
	}
	if member {
		if unclaimed == nil {
			return createdKey{}, nil, fmt.Errorf("you have no subscription key for this machine, "+
				"and only an administrator can issue one; ask one to run: keera key create "+
				"--subscription --user %s --name \"%s's Claude Code\", then run this again",
				me.Email, strings.Split(me.Email, "@")[0])
		}
		rotated, err := rotateFor(ctx, c, unclaimed.ID, name)
		return rotated, unclaimed, err
	}
	var created createdKey
	err = c.do(ctx, "POST", "/v1/keys", map[string]string{
		"org_id": orgID, "project_id": inProject, "user_id": me.UserID, "name": name,
		"kind": string(policy.KeySubscription),
	}, &created)
	return created, nil, err
}

// rotateFor rotates a key and returns the new one. An empty name keeps the
// old one's.
func rotateFor(ctx context.Context, c *client, keyID, name string) (createdKey, error) {
	var rotated createdKey
	err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(keyID)+"/rotate",
		map[string]string{"name": name}, &rotated)
	return rotated, err
}

// machineKeyName is the name of a machine's subscription key. The id tells
// apart machines that share a hostname, such as copies of one VM image, so one
// never replaces another's key.
func machineKeyName(host, machine string) string {
	return "claude-code on " + host + " (" + machine + ")"
}

// machineKeyNameRE matches what machineKeyName makes.
var machineKeyNameRE = regexp.MustCompile(`^claude-code on .+ \([0-9a-f]{8}\)$`)

// isMachineKeyName reports whether a key is some machine's already, so taking
// it over would break that machine.
func isMachineKeyName(name string) bool {
	return machineKeyNameRE.MatchString(name)
}

// machineID names this machine for its subscription key. It is random, made
// on first use, and kept next to the sign-in in keera's configuration
// directory.
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

// claudeConfigDir is Claude Code's configuration directory. CLAUDE_CONFIG_DIR
// moves it, as it does for Claude Code itself.
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

// managedSettingsDir is where Claude Code reads managed settings files from.
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

// managedBaseURL is the address Claude Code's managed settings send it to, or
// "" when they set none. serverCache is Claude Code's copy of the settings
// from the claude.ai admin console, which win. Then come the files in
// systemDir, where a later drop-in wins over an earlier one. Settings from MDM
// profiles and the Windows registry are not read.
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
// A file that is missing or cannot be read sets nothing.
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

// readSettings reads a settings file, or an empty one when there is none yet.
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

// setClaudeEnv writes the gateway's settings into the 'env' block, and says
// what else in the file would stop them working. An empty value removes the
// setting. Other headers in ANTHROPIC_CUSTOM_HEADERS are kept.
func setClaudeEnv(settings map[string]any, env map[string]string, key string) []string {
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
	block["ANTHROPIC_CUSTOM_HEADERS"] = strings.Join(append(headers, gateway.KeyHeader+": "+key), "\n")

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

// writeSettings replaces the file in one step, so Claude Code never reads half
// of it. The file holds a key, so only its owner may read it.
func writeSettings(path string, settings map[string]any) error {
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
