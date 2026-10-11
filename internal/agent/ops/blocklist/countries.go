package blocklist

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	bl "github.com/cloudground-it/cloudground/internal/blocklist"
)

// The country data (SPEC §5, #25): DB-IP's "IP to Country Lite", CC BY 4.0,
// downloaded without an account or a key and kept for a month.

const (
	// DefaultGeoDir holds the downloaded file.
	DefaultGeoDir = "/var/lib/cloudground-agent/geo"
	geoFile       = "dbip-country-lite.csv.gz"
	// geoMaxAge is how old the file may be before a render tries a newer
	// one: DB-IP publishes once a month.
	geoMaxAge = 35 * 24 * time.Hour
	// geoMaxBytes bounds the download (the file is about 5 MB) and what it
	// unpacks to (about 31 MB).
	geoMaxBytes   = 64 << 20
	geoMaxCSV     = 256 << 20
	geoURLPattern = "https://download.db-ip.com/free/dbip-country-lite-%s.csv.gz"
)

// geoURL is the month's file (YYYY-MM).
func (o *Ops) geoURL(month string) string {
	if o.GeoURL != nil {
		return o.GeoURL(month)
	}
	return fmt.Sprintf(geoURLPattern, month)
}

func (o *Ops) geoPath() string {
	dir := o.GeoDir
	if dir == "" {
		dir = DefaultGeoDir
	}
	return filepath.Join(dir, geoFile)
}

// CountryPrefixes returns the prefixes of the countries codes, from the
// data file, which it downloads first when it is missing or older than a
// month (SPEC §5). A refresh that fails keeps the file there is.
func (o *Ops) CountryPrefixes(ctx context.Context, codes []string) ([]netip.Prefix, error) {
	want := map[string]bool{}
	for _, c := range codes {
		if !bl.ValidCountry(c) {
			return nil, agentrpc.Invalid("%q is not a country code", c)
		}
		want[c] = true
	}
	o.geoMu.Lock()
	defer o.geoMu.Unlock()
	path := o.geoPath()
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > geoMaxAge {
		if derr := o.downloadGeo(ctx, path); derr != nil {
			if err != nil {
				return nil, agentrpc.Wrap(agentrpc.CodeUnavailable, "could not download the country data from db-ip.com", derr)
			}
			o.log().Warn("the country data could not be refreshed; the old copy is used", "err", derr)
		}
		if fi, err = os.Stat(path); err != nil {
			return nil, err
		}
	}
	key := fi.ModTime().String() + "|" + strings.Join(sortedKeys(want), ",")
	if o.geoCacheKey == key {
		return o.geoCache, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byCountry, err := readGeo(f, want)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, c := range sortedKeys(want) {
		out = append(out, byCountry[c]...)
	}
	o.geoCacheKey, o.geoCache = key, out
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// readGeo unpacks and reads the data file.
func readGeo(r io.Reader, want map[string]bool) (map[string][]netip.Prefix, error) {
	z, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("country data: %w", err)
	}
	defer z.Close()
	return bl.CountryPrefixes(io.LimitReader(z, geoMaxCSV), want)
}

// downloadGeo fetches this month's file, or last month's when this one is
// not out yet, checks that it reads, and puts it in place atomically.
func (o *Ops) downloadGeo(ctx context.Context, path string) error {
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	now := time.Now().UTC()
	var errs []error
	for _, month := range []string{now.Format("2006-01"), now.AddDate(0, -1, 0).Format("2006-01")} {
		body, err := fetch(ctx, client, o.geoURL(month))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := readGeo(bytes.NewReader(body), map[string]bool{}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", month, err))
			continue
		}
		if err := rootfile.MkdirAllMode(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	return errors.Join(errs...)
}

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, geoMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > geoMaxBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, geoMaxBytes)
	}
	return body, nil
}
