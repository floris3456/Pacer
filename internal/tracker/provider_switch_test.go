package tracker

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func providerGuardStore(t *testing.T, path string) (*sql.DB, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := Open(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	return db, s
}

func providerGuardClaim() Claim {
	return Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "mixed-alias", Request: "logical", Kind: "primary", Capability: Digest("shared-test-capability"), Auto: true, CompactOnSwitch: true, Checkpoint: "old"}
}

func providerGuardSelection(caller, provider string) Selection {
	return Selection{Caller: caller, Provider: provider, Auth: provider + "-a", Account: provider + "-account", Model: "model-one", Forced: true}
}

func requireProviderRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != "quota_provider_switch_forbidden" {
		t.Fatalf("expected provider-switch refusal, got %v", err)
	}
}

func TestAutomaticProviderGuardSurvivesRestart(t *testing.T) {
	for _, initial := range []string{"claude", "codex"} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			db, s := providerGuardStore(t, path)
			q := providerGuardClaim()
			first := providerGuardSelection("caller", initial)
			key := Key(first.Caller, initial, q)
			if _, err := s.Admit("initial", key, first, &q, true); err != nil {
				t.Fatal(err)
			}
			s.Complete("initial")
			db.Close()
			_, s = providerGuardStore(t, path)
			target := "claude"
			if initial == target {
				target = "codex"
			}
			q.Request, q.Checkpoint, q.Fresh = "next", "new-checkpoint", true
			otherKey := Key(first.Caller, target, q)
			_, err := s.Admit("cross-provider", otherKey, providerGuardSelection(first.Caller, target), &q, false)
			requireProviderRefusal(t, err)
			if b, active, err := s.Get(otherKey); err != nil || b.Key != "" || active {
				t.Fatal("refusal created a foreign-provider binding or flight", b, active, err)
			}
			if b, _, err := s.Get(key); err != nil || b.Provider != initial || b.Generation != 1 {
				t.Fatal("refusal altered the original binding", b, err)
			}
		})
	}
}

func TestAutomaticProviderGuardSerializesFirstAdmissions(t *testing.T) {
	_, s := providerGuardStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	q := providerGuardClaim()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, provider := range []string{"claude", "codex"} {
		go func() {
			<-start
			_, err := s.Admit(provider, Key("caller", provider, q), providerGuardSelection("caller", provider), &q, false)
			results <- err
		}()
	}
	close(start)
	accepted, refused := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		} else {
			requireProviderRefusal(t, err)
			refused++
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent first admissions: accepted=%d refused=%d", accepted, refused)
	}
}

func TestAutomaticProviderGuardScopesClaimIdentity(t *testing.T) {
	for _, scope := range []string{"caller", "installation", "session", "route"} {
		t.Run(scope, func(t *testing.T) {
			_, s := providerGuardStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
			q := providerGuardClaim()
			if _, err := s.Admit("first", Key("caller", "claude", q), providerGuardSelection("caller", "claude"), &q, true); err != nil {
				t.Fatal(err)
			}
			caller := "caller"
			switch scope {
			case "caller":
				caller = "other-caller"
			case "installation":
				q.Installation = "other-installation"
			case "session":
				q.Session = "other-session"
			case "route":
				q.Route = "other-route"
			}
			// Deliberately reuse the capability: it does not define this boundary.
			if _, err := s.Admit("independent", Key(caller, "codex", q), providerGuardSelection(caller, "codex"), &q, true); err != nil {
				t.Fatal("unrelated claim inherited another provider binding", err)
			}
		})
	}
}

func TestAutomaticProviderGuardPreservesSameProviderSwitches(t *testing.T) {
	_, s := providerGuardStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	q := providerGuardClaim()
	key := Key("caller", "claude", q)
	first := providerGuardSelection("caller", "claude")
	if _, err := s.Admit("first", key, first, &q, true); err != nil {
		t.Fatal(err)
	}
	next := first
	next.Auth, next.Account, next.Model = "claude-b", "another-claude-account", "model-two"
	q.Request, q.Checkpoint = "next", "new-checkpoint"
	b, err := s.Admit("same-provider", key, next, &q, true)
	if err != nil || b.Auth != next.Auth || b.Model != next.Model || b.Generation != 2 {
		t.Fatal("legitimate same-provider move failed", b, err)
	}
}

func TestAutomaticProviderGuardPreservesManualAndAuxiliaryChoices(t *testing.T) {
	_, s := providerGuardStore(t, filepath.Join(t.TempDir(), "state.sqlite"))
	q := providerGuardClaim()
	if _, err := s.Admit("first", Key("caller", "claude", q), providerGuardSelection("caller", "claude"), &q, false); err != nil {
		t.Fatal(err)
	}
	q.Kind = "compaction"
	if _, err := s.Admit("helper", Key("caller", "codex", q), providerGuardSelection("caller", "codex"), &q, false); err != nil {
		t.Fatal("explicit auxiliary choice was blocked", err)
	}
	if b, _, err := s.Get(Key("caller", "codex", q)); err != nil || b.Key != "" {
		t.Fatal("auxiliary created a provider binding", b, err)
	}
	q.Kind, q.Auto = "primary", false
	if _, err := s.Admit("manual", Key("caller", "codex", q), providerGuardSelection("caller", "codex"), &q, false); err != nil {
		t.Fatal("explicit manual choice was blocked", err)
	}
	q.Auto = true
	for _, provider := range []string{"claude", "codex"} {
		_, err := s.Admit("ambiguous-"+provider, Key("caller", provider, q), providerGuardSelection("caller", provider), &q, false)
		requireProviderRefusal(t, err)
	}
}
