package historyreports

import (
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func TestNativeReadingsBecomeCPAMPObservations(t *testing.T) {
	remaining := 60.0
	reset := time.UnixMilli(1791000000000)
	at := reset.Add(-2 * time.Hour)
	r := NativeReading{ID: 42, Account: "native-a", Provider: "claude", Scope: "all", Seconds: 5 * 3600, Remaining: &remaining, Reset: reset, At: at, Source: "header"}
	o, ok := nativeObservation(r, "legacy-a")
	if !ok || o.ID != "native:42" || o.Account != "legacy-a" || o.Kind != "5h" || o.Scope != "all" || *o.Used != 40 || o.Reset != reset.UnixMilli() || o.Start != reset.UnixMilli()-5*3600*1000 || o.Duration != 5*3600*1000 || o.Observed != at.UnixMilli() || o.Source != "response_header" {
		t.Fatalf("%+v", o)
	}
	// The same window identity CPAMP gives its own readings of that window.
	if o.WindowID != cpamp.WindowID(cpamp.Observation{Account: "legacy-a", Kind: "5h", Scope: "all", Reset: reset.UnixMilli()}) {
		t.Fatal("window ID scheme differs")
	}
	r.Seconds, r.Source = 7*86400, "api"
	if o, ok = nativeObservation(r, "legacy-a"); !ok || o.Kind != "weekly" || o.Source != "api_query" {
		t.Fatalf("%+v", o)
	}
	nan := 0.0 / func() float64 { return 0 }()
	over := 101.0
	for _, bad := range []NativeReading{
		{ID: 1, Provider: "claude", Scope: "all", Seconds: 3600, Remaining: &remaining, Reset: reset, At: at},
		{ID: 2, Provider: "claude", Scope: "all", Seconds: 18000, Reset: reset, At: at},
		{ID: 3, Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &remaining, At: at},
		{ID: 4, Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &remaining, Reset: reset},
		{ID: 5, Provider: "claude", Seconds: 18000, Remaining: &remaining, Reset: reset, At: at},
		{ID: 6, Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &nan, Reset: reset, At: at},
		{ID: 7, Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &over, Reset: reset, At: at},
	} {
		if _, ok := nativeObservation(bad, "legacy-a"); ok {
			t.Fatal("not a meter reading", bad.ID)
		}
	}
	if _, ok := nativeObservation(r, ""); ok {
		t.Fatal("reading without an account")
	}
}

func TestNativeReadingsUseTheirRequestsAccount(t *testing.T) {
	at := time.UnixMilli(1791000000000)
	r := NativeReading{Account: "n1", Provider: "claude", At: at}
	o := ForwardOrigins{Certificates: []ForwardOriginCertificate{
		{LegacyAccount: "legacy-1", Provider: "claude", SourceID: "src", NativeAccount: "n1", NativeCredentialRef: "c1", StartMS: at.UnixMilli() - 1000},
		{LegacyAccount: "legacy-1", Provider: "claude", SourceID: "src", NativeAccount: "n1", NativeCredentialRef: "c2", StartMS: at.UnixMilli() - 1000, EndMS: at.UnixMilli() + 1},
		{LegacyAccount: "legacy-2", Provider: "claude", SourceID: "src", NativeAccount: "n2", NativeCredentialRef: "c3", StartMS: 0, EndMS: at.UnixMilli()},
	}}
	if got, ok := readingAccount(o, "src", r); !ok || got != "legacy-1" {
		t.Fatal(got, ok)
	}
	// Outside every certificate: the native account reference requests get.
	r2 := NativeReading{Account: "n2", Provider: "claude", At: at}
	if got, ok := readingAccount(o, "src", r2); !ok || got != ForwardAccountRef("src", "claude", "n2") {
		t.Fatal(got, ok)
	}
	if got, _ := readingAccount(o, "src", NativeReading{Account: "n1", Provider: "codex", At: at}); got != ForwardAccountRef("src", "codex", "n1") {
		t.Fatal("certificate matched another provider", got)
	}
	o.Certificates = append(o.Certificates, ForwardOriginCertificate{LegacyAccount: "legacy-3", Provider: "claude", SourceID: "src", NativeAccount: "n1", NativeCredentialRef: "c4"})
	if _, ok := readingAccount(o, "src", r); ok {
		t.Fatal("disagreeing certificates must make the reading ambiguous")
	}
}
