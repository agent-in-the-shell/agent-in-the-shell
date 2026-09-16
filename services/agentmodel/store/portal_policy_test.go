package store

import (
	"context"
	"errors"
	"testing"
)

func TestPortalRotationFailsClosedAfterOperatorPolicyChange(t *testing.T) {
	for _, policy := range []string{"max_budget = 10", "expires_at = 1900000000", "budget_duration = '24h'"} {
		t.Run(policy, func(t *testing.T) {
			st, err := OpenSQLite(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			identity := PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
			_, err = st.CreatePortalKey(ctx, identity, ManagedKey{ID: "id", Name: identity.Email, KeyHash: "old", Models: []string{"chatgpt"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(`UPDATE api_keys SET ` + policy + ` WHERE id = 'id'`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.RotatePortalKey(ctx, identity, 1, "new"); !errors.Is(err, ErrPortalPolicy) {
				t.Fatal("policy bypass", err)
			}
			binding, err := st.GetPortalBinding(ctx, identity)
			if err != nil || binding.Revision != 1 {
				t.Fatal("failed rotation changed revision", binding, err)
			}
			if _, err := st.GetKeyByHash(ctx, "old"); err != nil {
				t.Fatal("failed rotation changed token", err)
			}
		})
	}
}
