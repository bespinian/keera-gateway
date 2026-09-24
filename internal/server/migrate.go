package server

import (
	"context"
	"fmt"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/config"
	"github.com/bespinian/keera-gateway/internal/store"
)

// migrate applies the schema and the model catalogue without starting a
// listener, for deployments that run migrations as their own step.
func migrate(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer st.Close()

	applied, err := st.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("schema is up to date")
	}
	for _, v := range applied {
		fmt.Println("applied", v)
	}

	if cfg.ModelsFile != "" {
		c, err := catalog.Apply(ctx, st, cfg.ModelsFile)
		if err != nil {
			return err
		}
		for _, m := range c.Models {
			fmt.Printf("catalogue %s -> %s %v\n", m.Alias, m.BackendModel, m.Backends)
		}
		for _, m := range c.MCPServers {
			fmt.Printf("catalogue mcp %s -> %s\n", m.Alias, m.URL)
		}
	}
	return nil
}
