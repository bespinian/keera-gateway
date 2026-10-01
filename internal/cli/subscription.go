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
	"strings"

	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Claude Code signed in to a Claude plan.
//
// `keera connect claude-code --subscription` issues this machine its own
// subscription key and writes it into Claude Code's settings, next to the
// gateway's address. The person never sees or copies the key. Running it again
// replaces the machine's key instead of adding another. See
// docs/subscriptions.md.

// subscriptionSetup is what one `connect --subscription` was asked for.
type subscriptionSetup struct {
	org, team, model string
	gatewayURL       string
	asJSON           bool
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
	env["ANTHROPIC_BASE_URL"] = in.gatewayURL

	// The settings are read before a key is issued, so a file that cannot be
	// changed leaves no key behind that nothing holds.
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, err := readSettings(path)
	if err != nil {
		return err
	}

	key, replaced, err := machineKey(ctx, c, orgID, me.UserID, in.team)
	if err != nil {
		return err
	}
	warnings := setClaudeEnv(settings, env, key.Key)
	if err := writeSettings(path, settings); err != nil {
		return fmt.Errorf("the key %s was issued, but %w; run this again to replace it", key.ID, err)
	}

	if in.asJSON {
		return out(true, map[string]any{
			"settings": path, "key_id": key.ID, "replaced": replaced, "model": alias,
			"url": in.gatewayURL, "warnings": warnings,
		}, nil)
	}
	fmt.Fprintf(os.Stderr, "%s · model %s · %s\n\n", styleErr.head("Claude Code with your Claude plan"),
		alias, in.gatewayURL)
	if replaced != "" {
		fmt.Fprintf(os.Stderr, "Replaced this machine's key %s with %s (%s…).\n", replaced, key.ID, key.Prefix)
	} else {
		fmt.Fprintf(os.Stderr, "Issued this machine the key %s (%s…).\n", key.ID, key.Prefix)
	}
	fmt.Fprintf(os.Stderr, "Wrote the gateway, the key and the models into %s.\n", path)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "\n%s\n", styleErr.warn(wrapAt(w, 0, 0, 76)))
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", wrapAt("Run `claude` in your project. If it is not signed in "+
		"to your Claude plan yet, run `/login` there. `/status` shows the gateway.", 0, 0, 76))
	return nil
}

// subscriptionEnv picks the models Claude Code is pointed at, and returns them
// as settings, with the main one's alias.
//
// Claude Code asks for an Opus, a Sonnet or a Haiku model by Anthropic's own
// names, which the gateway does not know. So each of the three is mapped to
// the subscription model of that family, or to the main one when there is none.
func subscriptionEnv(models []policy.Model, named string) (map[string]string, string, error) {
	var subs []policy.Model
	for _, m := range models {
		if m.Enabled && m.Subscription && m.Kind == policy.KindChat {
			subs = append(subs, m)
		}
	}
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
	env := map[string]string{"ANTHROPIC_MODEL": main.Alias}
	for _, family := range []string{"opus", "sonnet", "haiku"} {
		alias := main.Alias
		for _, m := range subs {
			if strings.Contains(m.BackendModel, family) {
				alias = m.Alias
				break
			}
		}
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(family)+"_MODEL"] = alias
	}
	return env, main.Alias, nil
}

// machineKey replaces this machine's subscription key, or issues the first one.
// It returns the new key and the id of the one it replaced, if any.
func machineKey(ctx context.Context, c *client, orgID, userID, team string,
) (createdKey, string, error) {
	machine, err := machineID()
	if err != nil {
		return createdKey{}, "", err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "this machine"
	}
	// The id tells apart machines that share a hostname, such as copies of
	// one VM image, so one never replaces another's key.
	alias := "claude-code on " + host + " (" + machine + ")"
	teamID, err := teamID(ctx, c, orgID, team)
	if err != nil {
		return createdKey{}, "", err
	}
	keys, err := list[store.KeySummary](ctx, c, inOrg("/v1/keys", orgID))
	if err != nil {
		return createdKey{}, "", err
	}
	for _, k := range keys {
		if k.UserID == userID && k.Kind == policy.KeySubscription && k.Alias == alias &&
			k.RevokedAt == nil {
			// A rotation keeps the key's team, so a different --team would be
			// ignored without a word.
			if team != "" && teamID != k.TeamID {
				return createdKey{}, "", fmt.Errorf("this machine already has the key %s, in "+
					"another team, and replacing it keeps that team; to move it, revoke it "+
					"first with: keera key revoke %s, then run this again", k.ID, k.ID)
			}
			var rotated createdKey
			err := c.do(ctx, "POST", "/v1/keys/"+url.PathEscape(k.ID)+"/rotate",
				map[string]string{}, &rotated)
			return rotated, k.ID, err
		}
	}
	var created createdKey
	err = c.do(ctx, "POST", "/v1/keys", map[string]string{
		"org_id": orgID, "team_id": teamID, "user_id": userID, "alias": alias,
		"kind": string(policy.KeySubscription),
	}, &created)
	return created, "", err
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
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("keeping this machine's id: %w", err)
	}
	return id, nil
}

// claudeSettingsPath is Claude Code's user settings file. CLAUDE_CONFIG_DIR
// moves it, as it does for Claude Code itself.
func claudeSettingsPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding Claude Code's settings: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
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
	if env, ok := settings["env"]; ok {
		if _, isObject := env.(map[string]any); !isObject {
			return nil, fmt.Errorf("the 'env' setting in %s is not an object, so it was left alone", path)
		}
	}
	return settings, nil
}

// setClaudeEnv writes the gateway's settings into the 'env' block, and says
// what else in the file would stop them working. Other headers in
// ANTHROPIC_CUSTOM_HEADERS are kept.
func setClaudeEnv(settings map[string]any, env map[string]string, key string) []string {
	block, _ := settings["env"].(map[string]any)
	if block == nil {
		block = map[string]any{}
		settings["env"] = block
	}
	for name, value := range env {
		block[name] = value
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
