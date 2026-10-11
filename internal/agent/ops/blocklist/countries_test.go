package blocklist

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// geoData is a country file in DB-IP's form, gzipped.
func geoData(t *testing.T, cn string) []byte {
	t.Helper()
	rows := []string{"1.0.0.0,1.0.0.255,AU", cn + ",CN"}
	for i := range 1000 {
		rows = append(rows, fmt.Sprintf("10.0.%d.%d,10.0.%d.%d,IT", i/256, i%256, i/256, i%256))
	}
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write([]byte(strings.Join(rows, "\n") + "\n"))
	z.Close()
	return b.Bytes()
}

// SPEC §5: the file is downloaded once (last month's when this one is
// missing), read for the codes asked, refreshed when older than a month,
// kept when the refresh fails, and its absence with no download is
// unavailable.
func TestCountryData(t *testing.T) {
	var hits atomic.Int32
	data := geoData(t, "1.0.1.0,1.0.3.255")
	thisMonth := time.Now().UTC().Format("2006-01")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, thisMonth) {
			http.NotFound(w, r) // not out yet
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	o := newOps(&host.FakeExec{})
	o.GeoDir = t.TempDir()
	o.GeoURL = func(month string) string { return srv.URL + "/dbip-country-lite-" + month + ".csv.gz" }
	ctx := context.Background()
	got, err := o.CountryPrefixes(ctx, []string{"CN"})
	if err != nil || fmt.Sprint(got) != "[1.0.1.0/24 1.0.2.0/23]" {
		t.Fatalf("CN = %v, %v", got, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("%d requests, want this month's then last month's", hits.Load())
	}
	if _, err := o.CountryPrefixes(ctx, []string{"CN"}); err != nil || hits.Load() != 2 {
		t.Fatalf("a fresh file was downloaded again (%d requests), %v", hits.Load(), err)
	}
	if _, err := o.CountryPrefixes(ctx, []string{"cn"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a lower-case code = %v", err)
	}

	// Old and the server failing: the old copy serves.
	path := filepath.Join(o.GeoDir, geoFile)
	old := time.Now().Add(-40 * 24 * time.Hour)
	os.Chtimes(path, old, old)
	srv.Close()
	if got, err := o.CountryPrefixes(ctx, []string{"CN", "AU"}); err != nil || len(got) != 3 {
		t.Fatalf("with a failed refresh = %v, %v", got, err)
	}
	// No file and no server: unavailable.
	os.Remove(path)
	if _, err := o.CountryPrefixes(ctx, []string{"CN"}); agentrpc.CodeOf(err) != agentrpc.CodeUnavailable {
		t.Fatalf("no data = %v", err)
	}
}

// SPEC §5: a download that is not the database is not kept.
func TestCountryDataRefusesGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>not gzip</html>")) }))
	defer srv.Close()
	o := newOps(&host.FakeExec{})
	o.GeoDir = t.TempDir()
	o.GeoURL = func(month string) string { return srv.URL + "/" + month }
	if _, err := o.CountryPrefixes(context.Background(), []string{"CN"}); err == nil {
		t.Fatal("garbage was taken for the country data")
	}
	if _, err := os.Stat(filepath.Join(o.GeoDir, geoFile)); err == nil {
		t.Fatal("garbage was kept")
	}
}
