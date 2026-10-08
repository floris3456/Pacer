package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// This opt-in acceptance consumes private SQLite backup snapshots, never the
// running lab database. No schema initializer, mutation or network call is used.
// An ordinary test run skips it; source fixtures are not native acceptance.
func TestNativePhysicalStoredSelections(t *testing.T) {
	path := os.Getenv("PACER_PHYSICAL_SNAPSHOT")
	if path == "" {
		t.Skip("requires an authorized native lab snapshot")
	}
	open := func(path string) *Store {
		t.Helper()
		abs, err := filepath.Abs(path)
		if err != nil || !strings.Contains(filepath.ToSlash(abs), "/physical-source-snapshots/") || filepath.Base(abs) == "pacer.sqlite" {
			t.Fatal("only private physical-source-snapshots copies are accepted")
		}
		u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}
		db, err := sql.Open("sqlite3", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		return &Store{db: db, clock: time.Now}
	}
	s := open(path)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT p.body FROM measurement_physical_proofs p JOIN measurement_physical_epochs e ON e.id=p.epoch_id JOIN measurement_source_consumers c ON c.id=e.consumer_id WHERE c.active=1 ORDER BY p.id LIMIT 8193`)
	if err != nil {
		t.Fatal(err)
	}
	var manifests []physicalManifest
	for rows.Next() {
		var raw []byte
		var m physicalManifest
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(manifests) == 0 || len(manifests) > 8192 {
		t.Fatalf("missing/bounded native proofs: count=%d err=%v", len(manifests), err)
	}
	seen := map[string]bool{}
	ttls := map[TTL]bool{}
	streams := map[bool]bool{}
	var nonempty []physicalManifest
	for _, m := range manifests {
		result, err := s.SelectPhysical(ctx, m.Selection)
		if err != nil || !result.Known || result.ProofID != m.ProofID {
			t.Fatalf("native exact selection refused: reason=%s err=%v", result.Reason, err)
		}
		if len(result.Facts) != 0 {
			nonempty = append(nonempty, m)
		}
		foreign := m.Selection
		foreign.Through.Sequence++
		negative, err := s.SelectPhysical(ctx, foreign)
		if err != nil || negative.Known {
			t.Fatalf("unissued upper cut was not refused: known=%v err=%v", negative.Known, err)
		}
		for _, f := range result.Facts {
			seen[f.EventID] = true
			var raw []byte
			if err = s.db.QueryRowContext(ctx, `SELECT body FROM measurement_records WHERE event_id=?`, f.EventID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var r Record
			if err = json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			if r.Native == nil || r.Native.BillingMappingVerified {
				t.Fatal("native local cut cannot prove complete billing mapping")
			}
			if r.Activity.PrimaryPrefix || r.Activity.PrefixRef != "" || r.Activity.ProtectionTTL != "" || !r.Activity.Partial {
				t.Fatalf("native markers manufactured primary-prefix evidence: %+v", r.Activity)
			}
			if r.EndTimeBasis != "source_final_usage_observation" {
				t.Fatal("final usage observation was relabeled as producer end")
			}
			if r.Usage.CacheCreate5m == nil || *r.Usage.CacheCreate5m != 25 || r.Usage.CacheCreate1h == nil || *r.Usage.CacheCreate1h != 75 {
				t.Fatal("independent native 25/75 cache-write split changed")
			}
			ttls[r.Policy.SelectedTTL] = true
			streams[r.Native.Stream] = true
		}
	}
	if len(seen) < 3 || !ttls[TTL5m] || !ttls[TTL1h] || !streams[true] || !streams[false] {
		t.Fatalf("native managed lifecycle missing: events=%d ttls=%v streams=%v", len(seen), ttls, streams)
	}
	t.Logf("native exact-cut joins: %d proofs, %d distinct physical events; 1h/5m and stream/nonstream; foreign cut refused; billing/warmth unknown", len(manifests), len(seen))
	if after := os.Getenv("PACER_PHYSICAL_REVOKED_SNAPSHOT"); after != "" {
		post := open(after)
		revoked := 0
		for _, m := range nonempty {
			r, err := post.SelectPhysical(ctx, m.Selection)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Known {
				if r.Reason != "physical_incarnation_gap" {
					t.Fatalf("unexpected native refusal instead of origin gap: %s", r.Reason)
				}
				revoked++
			}
		}
		if revoked == 0 {
			t.Fatal("native failed dispatch did not revoke any previously known exact selection")
		}
		t.Logf("native origin-wide sticky revocation: %d previously known nonempty proofs refused after failed dispatch", revoked)
	}
}
