package store

import (
	"bytes"
	"testing"
)

func TestResealSecretsReplacesOnlyWhatItIsGiven(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")
	if _, err := st.Pool().Exec(ctx, `INSERT INTO identities (external_id, refresh_token)
		VALUES ('oidc:ada', 'old-ada'), ('oidc:bob', 'old-bob'), ('oidc:cy', NULL)`); err != nil {
		t.Fatal(err)
	}
	var seen []SealedValue
	n, err := st.ResealSecrets(ctx, func(v SealedValue) []byte {
		seen = append(seen, v)
		if v.ExternalID == "oidc:ada" {
			return []byte("new-ada")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("replaced %d, want 1", n)
	}
	var tokens []SealedValue
	for _, v := range seen {
		if v.Kind == SealedRefreshToken {
			tokens = append(tokens, v)
		}
	}
	if len(tokens) != 2 {
		t.Errorf("was shown %d refresh tokens, want the 2 stored", len(tokens))
	}
	for id, want := range map[string]string{"oidc:ada": "new-ada", "oidc:bob": "old-bob"} {
		var got []byte
		if err := st.Pool().QueryRow(ctx, "SELECT refresh_token FROM identities WHERE external_id = $1",
			id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}
