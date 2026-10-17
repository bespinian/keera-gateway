package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/config"
)

func TestTemplateModelsOnTheDeploymentKeyAreLockedAsTheAPILocksThem(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "models.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Platform: catalog.Platform{"anthropic": {APIKey: "sk-platform"}}}

	cfg.ModelsFile = write(t, `
models:
  - alias: frontier
    provider: anthropic
    backend_model: claude-opus-5
    backends: [https://proxy.example/v1]
`)
	tpl, err := loadTemplate(cfg, log)
	if err != nil {
		t.Fatalf("loadTemplate: %v", err)
	}
	m := tpl.Models[0]
	if !m.PlatformKey || len(m.Backends) != 1 || m.Backends[0] != "https://api.anthropic.com/v1" {
		t.Errorf("model = %+v, want it on the deployment's key and the provider's endpoint", m)
	}

	// One the price table does not know cannot be billed, so a new
	// organisation must not get it.
	cfg.ModelsFile = write(t, `
models:
  - alias: unknown
    provider: anthropic
    backend_model: claude-not-a-model
    input_micros_per_mtok: 1
    output_micros_per_mtok: 1
`)
	if _, err := loadTemplate(cfg, log); err == nil || !strings.Contains(err.Error(), "price table") {
		t.Errorf("err = %v, want the model refused for not being in the price table", err)
	}
}
