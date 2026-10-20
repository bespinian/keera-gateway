package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Claude Code on a Keera key, or signed in to a Claude plan.
//
// `keera connect claude-code` gives this machine its own key and writes it
// into Claude Code's settings, next to the gateway's address and the models
// the key may use. The person never copies the key. Running it again replaces
// the machine's key instead of adding another. --print shows the settings
// instead, for a machine this command does not run on. With --subscription,
// the key is a subscription key and Claude Code keeps its Claude sign-in; see
// docs/subscriptions.md.

// claudeSetup is what one `connect claude-code` was asked for.
type claudeSetup struct {
	org, project, model string
	gatewayURL          string
	asJSON              bool
	subscription        bool
}

// connectClaude sets Claude Code on this machine up.
func connectClaude(ctx context.Context, c *client, in claudeSetup) error {
	me, err := whoami(ctx, c)
	if err != nil {
		return err
	}
	if in.subscription && !me.ClaudeSubscriptions {
		return errSubscriptionsOff
	}
	if me.UserID == "" {
		// The operator key wins over a sign-in, so signing in alone is not enough.
		if c.key != "" {
			hint := ""
			if !in.subscription {
				hint = ", or add --print to see the settings instead"
			}
			return errors.New("this machine's key belongs to a person, and the operator key is " +
				"nobody; unset KEERA_OPERATOR_KEY and sign in with 'keera login' to run this " +
				"as yourself" + hint)
		}
		return errors.New("this machine's key belongs to a person; sign in first with: keera login")
	}
	orgID, err := resolveOrg(ctx, c, in.org)
	if err != nil {
		return err
	}

	// The models are checked before a key is issued, so a mistyped name leaves
	// no key behind.
	var (
		models []policy.Model
		env    map[string]string
		alias  string
	)
	kind := policy.KeyStandard
	if in.subscription {
		kind = policy.KeySubscription
		all, err := catalogue(ctx, c, orgID)
		if err != nil {
			return err
		}
		if env, alias, err = subscriptionEnv(all, in.model); err != nil {
			return err
		}
		models = subscriptionModels(all)
	} else {
		if models, err = connectModels(ctx, c, orgID); err != nil {
			return err
		}
		if len(models) == 0 {
			return errNoChatModel
		}
		if _, ok := findModel(models, in.model); in.model != "" && !ok {
			return fmt.Errorf("no enabled chat model or router %s; this organisation has: %s",
				in.model, strings.Join(aliasesOf(models), ", "))
		}
	}
	dir, err := claudeConfigDir()
	if err != nil {
		return err
	}
	// Claude Code stops fetching the organisation's settings from Anthropic when
	// the user's own settings name an address. So when managed settings set it
	// already, it is left to them, and an earlier copy is taken out.
	managed := managedBaseURL(filepath.Join(dir, "remote-settings.json"), managedSettingsDir())

	// The settings are read before a key is issued, so a file that cannot be
	// changed leaves no key behind that nothing holds.
	path := filepath.Join(dir, "settings.json")
	settings, err := readSettings(path)
	if err != nil {
		return err
	}

	step, err := planMachineKey(ctx, c, orgID, me, in.project, kind)
	if err != nil {
		return err
	}
	replaced := step.replaces
	if !in.subscription && replaced != nil {
		// Replacing a key revokes it at once, and activating one shows its
		// secret only once, so what it may use is checked before. The key
		// keeps its guardrails either way.
		if models, env, alias, err = keyEnv(ctx, c, orgID, replaced.ID, models, in.model); err != nil {
			return err
		}
	}
	key, err := step.issue(ctx, c)
	if err != nil {
		return err
	}
	subscriptionKey := ""
	if in.subscription {
		subscriptionKey = key.Key
		// A Keera key written there without --subscription would be sent
		// instead of the Claude sign-in.
		if block, _ := settings["env"].(map[string]any); block != nil {
			if token, _ := block["ANTHROPIC_AUTH_TOKEN"].(string); strings.HasPrefix(token, auth.Prefix) {
				env["ANTHROPIC_AUTH_TOKEN"] = ""
			}
		}
	} else {
		if env == nil {
			// A new key's models are only known once it is issued, so one that
			// may use none of them is revoked again.
			if models, env, alias, err = keyEnv(ctx, c, orgID, key.ID, models, in.model); err != nil {
				if rerr := c.do(ctx, "DELETE", "/v1/keys/"+url.PathEscape(key.ID), nil, nil); rerr != nil {
					return fmt.Errorf("%w; revoking the key %s again: %w", err, key.ID, rerr)
				}
				return fmt.Errorf("%w; the new key was revoked again", err)
			}
		}
		env["ANTHROPIC_AUTH_TOKEN"] = key.Key
		// Claude Code's telemetry holds no prompts, but it should not leave
		// through anything but the gateway.
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		// Set by --subscription. A Keera key may reach models that are not
		// Anthropic's, which have no tool search.
		env["ENABLE_TOOL_SEARCH"] = ""
	}
	env["ANTHROPIC_BASE_URL"] = in.gatewayURL
	if managed != "" {
		env["ANTHROPIC_BASE_URL"] = ""
	}
	warnings := setClaudeEnv(settings, env, subscriptionKey)
	setModelOverrides(settings, claudeModelOverrides(models))
	if managed != "" && strings.TrimRight(managed, "/") != strings.TrimRight(in.gatewayURL, "/") {
		warnings = append(warnings, "Your organisation's managed settings send Claude Code to "+
			managed+", not to "+in.gatewayURL+". The key works only if both are this gateway.")
	}
	if err := writeJSONFile(path, settings); err != nil {
		return fmt.Errorf("the key %s was issued, but writing %s: %w; run this again to replace it",
			key.ID, path, err)
	}

	if in.asJSON {
		replacedID := ""
		if replaced != nil && !replaced.Pending {
			replacedID = replaced.ID
		}
		return out(true, map[string]any{
			"settings": path, "key_id": key.ID, "replaced": replacedID, "model": alias,
			"models": aliasesOf(models), "url": in.gatewayURL, "url_managed": managed != "",
			"warnings": warnings,
		}, nil)
	}
	head, next := "Claude Code", "It starts with "+alias+". '/model <name>' switches to another of "+
		strings.Join(aliasesOf(models), ", ")+", and '/status' shows the gateway."
	if in.subscription {
		head, next = "Claude Code with your Claude plan", "If it is not signed in to your "+
			"Claude plan yet, run '/login' there. '/status' shows the gateway."
	}
	fmt.Fprintf(os.Stderr, "%s · model %s · %s\n\n", styleErr.head(head), alias, in.gatewayURL)
	saidKey(key, replaced)
	if managed != "" {
		fmt.Fprintf(os.Stderr, "Wrote the key and the models into %s. The gateway's address "+
			"comes from your organisation's managed settings.\n", path)
	} else {
		fmt.Fprintf(os.Stderr, "Wrote the gateway, the key and the models into %s.\n", path)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "\n%s\n", styleErr.warn(wrapAt(w, 0, 0, 76)))
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", wrapAt("Run 'claude' in your project. "+next, 0, 0, 76))
	return nil
}

// keyEnv narrows models to those the key may use, and points Claude Code at
// them: the one named, or else the first.
func keyEnv(ctx context.Context, c *client, orgID, keyID string, models []policy.Model,
	named string,
) ([]policy.Model, map[string]string, string, error) {
	// Not connectKey, which takes only active keys: this machine's key may
	// have expired, and that is usually why people run this again.
	var access struct {
		Keys []ownKey `json:"keys"`
	}
	if err := c.do(ctx, "GET", "/v1/access", nil, &access); err != nil {
		return nil, nil, "", err
	}
	i := slices.IndexFunc(access.Keys, func(k ownKey) bool {
		return k.ID == keyID && k.OrgID == orgID
	})
	if i < 0 {
		return nil, nil, "", fmt.Errorf("the key %s is not yours", keyID)
	}
	if models = keyModels(models, access.Keys[i].AllowedModels); len(models) == 0 {
		return nil, nil, "", fmt.Errorf("the key %s may use no chat model, so nothing was "+
			"written; an administrator can allow one in its guardrails, then run this again", keyID)
	}
	env, alias, ok := claudeModelEnv(models, named)
	if !ok {
		return nil, nil, "", fmt.Errorf("the key %s may not use %s, so nothing was written; "+
			"it may use: %s", keyID, named, strings.Join(aliasesOf(models), ", "))
	}
	return models, env, alias, nil
}
