// Package traffic reads each site's access log incrementally into its
// visitor statistics (#33, SPEC.md), on the panel's schedule and never on
// a request's path.
package traffic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/traffic"
)

// DefaultDir holds the aggregates, one file per site.
const DefaultDir = "/var/lib/cloudground-agent/traffic"

// DefaultMaxRead bounds what one round reads of one log (SPEC §1).
const DefaultMaxRead = 32 << 20

// headLen is how much of a log's start tells one log from the next.
const headLen = 64

// Ops collects and answers the statistics.
type Ops struct {
	LogRoot string // each site's logs, <LogRoot>/<domain>/access.log
	Dir     string // "" is DefaultDir
	MaxRead int64  // 0 is DefaultMaxRead
	Now     func() time.Time
	Log     *slog.Logger

	mu sync.Mutex // one round, or one read of a file, at a time
}

// Register wires the methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.TrafficCollect, o.Collect)
	agentrpc.Handle(s, agentrpc.TrafficStats, o.Stats)
}

// state is one site's file: where its log was left, and its aggregate.
type state struct {
	Domain string `json:"domain"`
	Offset int64  `json:"offset"`
	Inode  uint64 `json:"inode"`
	// HeadLen and HeadSum are the length and SHA-256 of the log's first
	// bytes (at most headLen) when it was read: a log truncated and written
	// past the offset again before a round has another head. A digest, so
	// the file keeps no address.
	HeadLen     int64         `json:"head_len"`
	HeadSum     string        `json:"head_sum"`
	CollectedAt time.Time     `json:"collected_at"`
	Site        *traffic.Site `json:"site"`
}

func (o *Ops) dir() string {
	if o.Dir != "" {
		return o.Dir
	}
	return DefaultDir
}

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) maxRead() int64 {
	if o.MaxRead > 0 {
		return o.MaxRead
	}
	return DefaultMaxRead
}

func (o *Ops) file(id int64) string {
	return filepath.Join(o.dir(), strconv.FormatInt(id, 10)+".json")
}

func (o *Ops) load(id int64) (*state, error) {
	b, err := os.ReadFile(o.file(id))
	if errors.Is(err, fs.ErrNotExist) {
		return &state{Site: &traffic.Site{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		// A damaged file starts again rather than stopping the site's statistics.
		o.log().Warn("traffic: a damaged state file is started again", "site", id, "err", err)
		return &state{Site: &traffic.Site{}}, nil
	}
	if st.Site == nil {
		st.Site = &traffic.Site{}
	}
	return &st, nil
}

func (o *Ops) save(id int64, st *state) error {
	if err := os.MkdirAll(o.dir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(o.dir(), ".traffic-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err == nil {
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), o.file(id))
}

// Collect reads what every site's log gained (SPEC §1).
func (o *Ops) Collect(ctx context.Context, p rpc.TrafficCollectParams) (rpc.TrafficCollectResult, error) {
	if err := p.Validate(); err != nil {
		return rpc.TrafficCollectResult{}, agentrpc.Invalid("%s", err.Error())
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := rpc.TrafficCollectResult{Sites: make([]rpc.TrafficCollected, 0, len(p.Sites))}
	keep := map[string]bool{}
	for _, t := range p.Sites {
		keep[strconv.FormatInt(t.ID, 10)+".json"] = true
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		row, err := o.collectSite(t)
		if err != nil {
			row.Error = err.Error()
			o.log().Warn("traffic: could not read a site's log", "site", t.Domain, "err", err)
		}
		out.Sites = append(out.Sites, row)
	}
	// The files of sites the panel no longer has.
	entries, _ := os.ReadDir(o.dir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !keep[e.Name()] {
			if err := os.Remove(filepath.Join(o.dir(), e.Name())); err != nil {
				o.log().Warn("traffic: could not remove a dropped site's statistics", "file", e.Name(), "err", err)
			}
		}
	}
	return out, nil
}

// collectSite runs one site's round.
func (o *Ops) collectSite(t rpc.TrafficTarget) (rpc.TrafficCollected, error) {
	row := rpc.TrafficCollected{ID: t.ID}
	st, err := o.load(t.ID)
	if err != nil {
		return row, err
	}
	if st.Domain != "" && st.Domain != t.Domain {
		// Another site under this id (a restored record): start again.
		st = &state{Site: &traffic.Site{}}
	}
	st.Domain = t.Domain
	logPath := filepath.Join(o.LogRoot, t.Domain, "access.log")
	f, ino, size, err := openLog(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return row, nil // nothing logged yet
	}
	if err != nil {
		return row, err
	}
	defer f.Close()
	now := o.now()
	add := func(data []byte) {
		for len(data) > 0 {
			var l []byte
			l, data, _ = bytes.Cut(data, []byte{'\n'})
			if len(l) == 0 {
				continue
			}
			if h, ok := traffic.ParseLine(string(l)); ok {
				st.Site.Add(h, now)
				row.Lines++
			} else {
				row.Skipped++
			}
		}
	}
	budget := o.maxRead()
	sameHead := st.HeadLen == 0 || headSum(f, st.HeadLen) == st.HeadSum
	// Rotated (SPEC §1 step 3): the rest of the old log first, then the new from 0.
	if (st.Inode != 0 && ino != st.Inode) || size < st.Offset || !sameHead {
		if old, oino, osize, err := openLog(logPath + ".1"); err == nil {
			if osize >= st.Offset && (ino == st.Inode || oino == st.Inode) {
				data, err := readLines(old, st.Offset, min(osize-st.Offset, budget))
				if err == nil {
					add(data)
					row.Read += int64(len(data))
					budget -= int64(len(data))
				}
			}
			old.Close()
		}
		st.Offset = 0
	}
	st.Inode = ino
	if budget > 0 && size > st.Offset {
		n := min(size-st.Offset, budget)
		data, err := readLines(f, st.Offset, n)
		if err != nil {
			return row, err
		}
		if len(data) == 0 && n == budget {
			// One line longer than the whole read: skipped.
			st.Offset += n
			row.Read += n
			row.Skipped++
		} else {
			add(data)
			st.Offset += int64(len(data))
			row.Read += int64(len(data))
		}
	}
	st.HeadLen = min(st.Offset, int64(headLen))
	st.HeadSum = headSum(f, st.HeadLen)
	st.Site.Settle(now)
	st.CollectedAt = now.UTC()
	return row, o.save(t.ID, st)
}

// openLog opens a log without following a link; it must be a regular file.
func openLog(path string) (*os.File, uint64, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = fmt.Errorf("%s is not a regular file", path)
		}
		return nil, 0, 0, err
	}
	var ino uint64
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(s.Ino)
	}
	return f, ino, fi.Size(), nil
}

// headSum is the SHA-256 of a log's first n bytes ("" for none).
func headSum(f *os.File, n int64) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	got, _ := f.ReadAt(buf, 0)
	sum := sha256.Sum256(buf[:got])
	return hex.EncodeToString(sum[:])
}

// readLines reads n bytes at off and keeps them up to the last line feed.
func readLines(f *os.File, off, n int64) ([]byte, error) {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:got]
	i := bytes.LastIndexByte(buf, '\n')
	return buf[:i+1], nil
}

// Stats answers a site's range (SPEC §2).
func (o *Ops) Stats(_ context.Context, p rpc.TrafficStatsParams) (rpc.TrafficStatsResult, error) {
	if err := p.Validate(); err != nil {
		return rpc.TrafficStatsResult{}, agentrpc.Invalid("%s", err.Error())
	}
	o.mu.Lock()
	st, err := o.load(p.ID)
	o.mu.Unlock()
	if err != nil {
		return rpc.TrafficStatsResult{}, err
	}
	if st.Domain != "" && st.Domain != p.Domain {
		st = &state{Site: &traffic.Site{}}
	}
	res := rpc.TrafficStatsResult{Stats: st.Site.Stats(p.Days, o.now())}
	if !st.CollectedAt.IsZero() {
		at := st.CollectedAt
		res.CollectedAt = &at
	}
	return res, nil
}
