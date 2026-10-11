package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ResticOutputLimit bounds what the agent keeps of one restic command's
// stdout (SPEC: restic, Output). The commands whose output is read whole
// (snapshots, forget plans, check, prune) print kilobytes; a backup's
// progress is not kept at all.
const ResticOutputLimit = 16 << 20

// ErrOutputTooLarge is a restic command whose kept output passed
// ResticOutputLimit: what was kept is incomplete, so nothing may be decoded
// from it as if it were whole.
var ErrOutputTooLarge = errors.New("restic: output too large")

// ResticOutput is the stdout of one restic command as the agent keeps it:
// all of it up to limit bytes, except that for `backup --json` the
// `status` progress messages (restic prints dozens a second when it is not
// told --quiet) are dropped as they arrive, so a backup of hours holds only
// its summary and the other messages. Past limit, the rest is dropped and
// Err reports ErrOutputTooLarge.
type ResticOutput struct {
	limit      int
	dropStatus bool
	buf        []byte
	line       []byte // an unfinished line (dropStatus only)
	over       bool
}

// NewResticOutput returns the output of restic run with args, bounded by
// ResticOutputLimit.
func NewResticOutput(args []string) *ResticOutput {
	return newResticOutput(args, ResticOutputLimit)
}

func newResticOutput(args []string, limit int) *ResticOutput {
	backupJSON := len(args) > 0 && args[0] == "backup" && slices.Contains(args, "--json")
	return &ResticOutput{limit: limit, dropStatus: backupJSON}
}

// Write never fails: restic must not die of a full pipe because the agent
// stopped keeping its output.
func (o *ResticOutput) Write(p []byte) (int, error) {
	n := len(p)
	if !o.dropStatus {
		o.keep(p)
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(o.line)+len(p) > o.limit {
				o.over, o.line = true, o.line[:0]
				return n, nil
			}
			o.line = append(o.line, p...)
			return n, nil
		}
		o.line = append(o.line, p[:i+1]...)
		p = p[i+1:]
		if !isStatusLine(o.line) {
			o.keep(o.line)
		}
		o.line = o.line[:0]
	}
	return n, nil
}

func (o *ResticOutput) keep(p []byte) {
	if o.over {
		return
	}
	if len(o.buf)+len(p) > o.limit {
		o.over = true
		return
	}
	o.buf = append(o.buf, p...)
}

// isStatusLine reports whether line is one of backup's progress messages.
func isStatusLine(line []byte) bool {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || t[0] != '{' || !bytes.Contains(t, []byte(`"status"`)) {
		return false
	}
	var m struct {
		MessageType string `json:"message_type"`
	}
	return json.Unmarshal(t, &m) == nil && m.MessageType == "status"
}

// Bytes is the kept output, with a last line restic did not end.
func (o *ResticOutput) Bytes() []byte {
	if len(o.line) > 0 && !isStatusLine(o.line) {
		o.keep(o.line)
		o.line = o.line[:0]
	}
	return o.buf
}

// Err is ErrOutputTooLarge once the output passed its limit.
func (o *ResticOutput) Err() error {
	if o.over {
		return fmt.Errorf("%w (more than %d bytes kept)", ErrOutputTooLarge, o.limit)
	}
	return nil
}

// withOutputErr joins out's overflow, if any, to err.
func withOutputErr(err error, out *ResticOutput) error {
	if oerr := out.Err(); oerr != nil {
		return errors.Join(err, oerr)
	}
	return err
}
