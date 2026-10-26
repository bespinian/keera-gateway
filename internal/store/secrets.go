package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SealedKind is which rows a sealed value comes from, and so which name it was
// sealed under.
type SealedKind string

const (
	SealedModel        SealedKind = "model"
	SealedMCP          SealedKind = "mcp"
	SealedRefreshToken SealedKind = "refresh-token"
)

// SealedValue is one value encrypted with KEERA_SECRET_KEY, and what names it.
type SealedValue struct {
	Kind SealedKind
	// OrgID and Alias name a model's or an MCP server's credential,
	// ExternalID a refresh token.
	OrgID, Alias, ExternalID string
	Sealed                   []byte
}

// sealedColumns is every column that holds a sealed value. Each query selects
// two text columns naming the row, then the value; each update takes the same
// two and the new value.
var sealedColumns = []struct {
	kind          SealedKind
	query, update string
}{
	{
		SealedModel,
		"SELECT org_id, alias, api_key_ct FROM models WHERE api_key_ct IS NOT NULL FOR UPDATE",
		"UPDATE models SET api_key_ct = $3 WHERE org_id = $1 AND alias = $2",
	},
	{
		SealedMCP,
		"SELECT org_id, alias, api_key_ct FROM mcp_servers WHERE api_key_ct IS NOT NULL FOR UPDATE",
		"UPDATE mcp_servers SET api_key_ct = $3 WHERE org_id = $1 AND alias = $2",
	},
	{
		SealedRefreshToken,
		"SELECT external_id, '', refresh_token FROM identities WHERE refresh_token IS NOT NULL FOR UPDATE",
		"UPDATE identities SET refresh_token = $3 WHERE external_id = $1 AND $2 = ''",
	},
	{
		SealedRefreshToken,
		`SELECT external_id, encode(token_hash, 'hex'), refresh_token FROM signups
		WHERE refresh_token IS NOT NULL FOR UPDATE`,
		"UPDATE signups SET refresh_token = $3 WHERE external_id = $1 AND token_hash = decode($2, 'hex')",
	},
}

// ResealSecrets passes every sealed value to reseal and stores what it returns,
// unless nil. It returns how many values it replaced, in one transaction.
func (s *Store) ResealSecrets(ctx context.Context, reseal func(SealedValue) []byte) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		n = 0
		for _, c := range sealedColumns {
			type found struct {
				a, b   string
				sealed []byte
			}
			rows, err := tx.Query(ctx, c.query)
			if err != nil {
				return err
			}
			all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (found, error) {
				var f found
				return f, r.Scan(&f.a, &f.b, &f.sealed)
			})
			if err != nil {
				return err
			}
			for _, f := range all {
				v := SealedValue{Kind: c.kind, Sealed: f.sealed}
				if c.kind == SealedRefreshToken {
					v.ExternalID = f.a
				} else {
					v.OrgID, v.Alias = f.a, f.b
				}
				fresh := reseal(v)
				if fresh == nil {
					continue
				}
				if _, err := tx.Exec(ctx, c.update, f.a, f.b, fresh); err != nil {
					return err
				}
				n++
			}
		}
		return nil
	})
	return n, err
}
