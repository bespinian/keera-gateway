package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What a signed-in command line keeps, and where.
//
// One file holds a sign-in per gateway, keyed by the gateway's URL. The file
// is a credential: it is written 0600 in a 0700 directory, and replaced by
// rename so an interrupted write never leaves half a token.

// credentials is the file's contents.
type credentials struct {
	Gateways map[string]signIn `json:"gateways"`
	// Default is the gateway commands talk to when nothing else says which.
	// The last `keera login` sets it, so one login line is a whole setup.
	// `keera whoami` says which gateway is in use and why.
	Default string `json:"default,omitempty"`
	// Orgs is the organisation commands act on at each gateway, keyed like
	// Gateways. 'keera org switch' sets it.
	Orgs map[string]string `json:"orgs,omitempty"`
}

// signIn is one gateway's sign-in.
type signIn struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// expired reports whether this sign-in has run out. The gateway decides;
// this only saves a request that would fail.
func (c signIn) expired() bool {
	return !c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt)
}

// credentialsPath is the file. KEERA_CONFIG_DIR moves it, for a shared
// account or a pipeline.
func credentialsPath() (string, error) {
	if dir := os.Getenv("KEERA_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials.json"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the configuration directory: %w; "+
			"set KEERA_CONFIG_DIR to say where credentials should live", err)
	}
	return filepath.Join(dir, "keera", "credentials.json"), nil
}

// loadCredentials reads the file. A missing file means never signed in.
func loadCredentials() (credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentials{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return credentials{Gateways: map[string]signIn{}}, nil
	}
	if err != nil {
		return credentials{}, err
	}
	var c credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return credentials{}, fmt.Errorf("reading %s: %w; delete it and run 'keera login'", path, err)
	}
	if c.Gateways == nil {
		c.Gateways = map[string]signIn{}
	}
	return c, nil
}

// defaultGateway is the gateway this machine last signed in to, and empty on
// one that never has.
func defaultGateway() string {
	c, err := loadCredentials()
	if err != nil {
		return ""
	}
	return c.Default
}

// saveSignIn records a sign-in, leaving other gateways alone, and makes this
// gateway the default: signing in says clearly which one is meant.
func saveSignIn(base string, cred signIn) error {
	return updateCredentials(func(c *credentials) {
		c.Gateways[gatewayKey(base)] = cred
		c.Default = gatewayKey(base)
	})
}

// saveOrg records the organisation commands act on at a gateway. Empty
// forgets it.
func saveOrg(base, orgID string) error {
	return updateCredentials(func(c *credentials) {
		if c.Orgs == nil {
			c.Orgs = map[string]string{}
		}
		if orgID == "" {
			delete(c.Orgs, gatewayKey(base))
		} else {
			c.Orgs[gatewayKey(base)] = orgID
		}
	})
}

// forgetSignIn removes one gateway's sign-in, and the default with it when it
// pointed here, so later commands do not go where they cannot sign in.
func forgetSignIn(base string) error {
	return updateCredentials(func(c *credentials) {
		delete(c.Gateways, gatewayKey(base))
		delete(c.Orgs, gatewayKey(base))
		if c.Default == gatewayKey(base) {
			c.Default = ""
			// If exactly one gateway is left, it becomes the default. With more,
			// picking one would silently change which deployment commands act on.
			if len(c.Gateways) == 1 {
				for other := range c.Gateways {
					c.Default = other
				}
			}
		}
	})
}

func updateCredentials(change func(*credentials)) error {
	c, err := loadCredentials()
	if err != nil {
		return err
	}
	change(&c)
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	return writeJSONFile(path, c)
}

// writeJSONFile replaces the file at path with v in one step, so nothing ever
// reads half of it. The new contents are written beside it, so the rename
// stays on one filesystem and is atomic. The file holds a key, so only its
// owner may read it.
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// gatewayKey is the spelling a gateway is filed under, so that a trailing
// slash or a capital letter does not make one deployment look like two.
func gatewayKey(base string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
}
