package version

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The release feed (SPEC §4). UpdateURL is a repository on GitHub, or
// anything serving the same three paths:
//
//	<UpdateURL>/releases.atom                       the published releases (Atom)
//	<UpdateURL>/releases/download/<tag>/SHA256SUMS  a release's signed manifest
//	<UpdateURL>/releases/download/<tag>/<file>      a release's files
//
// The feed is unsigned and only proposes a tag: what may be installed is
// decided by the signed manifest of that tag, whose version line must be the
// tag (FetchManifest) and must suit the channel (CheckChannel).

// Feed reads the release feed with Client (http.DefaultClient when nil).
type Feed struct {
	Client *http.Client
}

// ErrNoManifest and ErrNoSignature wrap a failure to download a release's
// manifest or its signature (as opposed to one that does not verify).
var (
	ErrNoManifest  = errors.New("download the release manifest")
	ErrNoSignature = errors.New("download the release signature")
)

// Limits on what the feed may send.
const (
	maxFeed      = 1 << 20
	maxManifest  = 1 << 20
	maxSignature = 4 << 10
)

// ReleaseBase is the URL a release's files are downloaded from, without the
// trailing slash.
func ReleaseBase(tag string) string {
	return strings.TrimRight(UpdateURL, "/") + "/releases/download/" + url.PathEscape(tag)
}

// Latest returns the newest release tag the feed lists for channel.
func (f Feed) Latest(ctx context.Context, channel string) (string, error) {
	ch, ok := ParseChannel(channel)
	if !ok {
		return "", fmt.Errorf("unknown release channel %q", channel)
	}
	if UpdateURL == "" {
		return "", errNoOrigin
	}
	body, err := f.get(ctx, strings.TrimRight(UpdateURL, "/")+"/releases.atom", maxFeed)
	if err != nil {
		return "", fmt.Errorf("read the release feed: %w", err)
	}
	tags, err := feedTags(body)
	if err != nil {
		return "", err
	}
	best := ""
	for _, t := range tags {
		if CheckChannel(ch, t) != nil {
			continue
		}
		if best == "" || Newer(t, best) {
			best = t
		}
	}
	if best == "" {
		return "", fmt.Errorf("the release feed lists no %s release", ch)
	}
	return best, nil
}

// FetchManifest downloads and verifies the signed manifest of tag. Its
// version line must be tag itself, so a manifest served under another
// release's name is refused.
func (f Feed) FetchManifest(ctx context.Context, tag string) (Manifest, error) {
	if UpdateURL == "" {
		return Manifest{}, errNoOrigin
	}
	base := ReleaseBase(tag)
	body, err := f.get(ctx, base+"/"+ManifestName, maxManifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrNoManifest, err)
	}
	sig, err := f.get(ctx, base+"/"+SignatureName, maxSignature)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrNoSignature, err)
	}
	m, err := VerifyManifest(body, sig)
	if err != nil {
		return Manifest{}, err
	}
	if m.Version != tag {
		return Manifest{}, fmt.Errorf("the manifest published as %s is signed for %s: refusing it", tag, m.Version)
	}
	return m, nil
}

func (f Feed) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

// feedTags reads the release tags out of an Atom feed: each entry's link to
// .../releases/tag/<tag>, else the last segment of its id. Entries that name
// no release tag are skipped.
func feedTags(body []byte) ([]string, error) {
	var doc struct {
		Entries []struct {
			ID    string `xml:"id"`
			Links []struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the release feed is not an Atom feed: %w", err)
	}
	var tags []string
	for _, e := range doc.Entries {
		tag := ""
		for _, l := range e.Links {
			if _, t, ok := strings.Cut(l.Href, "/releases/tag/"); ok {
				tag, _ = url.PathUnescape(t)
				break
			}
		}
		if tag == "" {
			tag = e.ID[strings.LastIndex(e.ID, "/")+1:]
		}
		if _, ok := ReleaseTag(tag); ok {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}
