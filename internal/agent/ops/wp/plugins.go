package wp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Plugins lists the installed plugins and themes with their update state
// (SPEC §3).
func (o *Ops) Plugins(ctx context.Context, p rpc.WPParams) (rpc.WPPluginsResult, error) {
	var res rpc.WPPluginsResult
	for _, kind := range []string{"plugin", "theme"} {
		out, err := o.Run(ctx, p.Site, kind, "list", "--format=json", "--fields=name,status,version,update")
		if err != nil {
			return rpc.WPPluginsResult{}, fmt.Errorf("wp %s list: %w", kind, err)
		}
		items, err := parseList(out)
		if err != nil {
			return rpc.WPPluginsResult{}, fmt.Errorf("wp %s list: %w", kind, err)
		}
		if kind == "plugin" {
			res.Plugins = items
		} else {
			res.Themes = items
		}
	}
	return res, nil
}

// parseList decodes wp-cli's JSON list. Anything a plugin printed before
// the array (a notice on stdout) is skipped. wp-cli gives "update" as a
// string ("available", "none", "unavailable", "version higher than
// expected") for a plugin or theme it can check, and as the JSON boolean
// false for one it cannot (a must-use plugin, a drop-in): false reads as
// "none", true as "available" (SPEC §3).
func parseList(out []byte) ([]rpc.WPPlugin, error) {
	i := bytes.IndexByte(out, '[')
	if i < 0 {
		return nil, errors.New("no JSON list in the output")
	}
	var raw []struct {
		Name    string          `json:"name"`
		Status  string          `json:"status"`
		Version json.RawMessage `json:"version"`
		Update  json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out[i:]), &raw); err != nil {
		return nil, err
	}
	items := make([]rpc.WPPlugin, 0, len(raw))
	for _, r := range raw {
		update, err := updateState(r.Update)
		if err != nil {
			return nil, fmt.Errorf("%s: update: %w", r.Name, err)
		}
		version, err := looseString(r.Version)
		if err != nil {
			return nil, fmt.Errorf("%s: version: %w", r.Name, err)
		}
		items = append(items, rpc.WPPlugin{Name: r.Name, Status: r.Status, Version: version, Update: update})
	}
	return items, nil
}

// updateState is wp-cli's "update" field as a string; absent or empty is
// "none".
func updateState(v json.RawMessage) (string, error) {
	var b bool
	if json.Unmarshal(v, &b) == nil {
		if b {
			return "available", nil
		}
		return "none", nil
	}
	s, err := looseString(v)
	if s == "" && err == nil {
		s = "none"
	}
	return s, err
}

// looseString reads a JSON string, a number (a version a plugin header
// left unquoted) or null/false/absent as "".
func looseString(v json.RawMessage) (string, error) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" || string(v) == "false" {
		return "", nil
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return "", fmt.Errorf("unexpected value %s", v)
	}
	return n.String(), nil
}

// updateSteps are the wp-cli commands of each update target.
var updateSteps = map[string][][]string{
	"core":    {{"core", "update"}, {"core", "update-db"}},
	"plugins": {{"plugin", "update", "--all"}},
	"themes":  {{"theme", "update", "--all"}},
	"all":     {{"core", "update"}, {"core", "update-db"}, {"plugin", "update", "--all"}, {"theme", "update", "--all"}},
}

// Update updates core, plugins, themes or all of them, then makes the live
// site run the new code with fresh caches (SPEC §4).
func (o *Ops) Update(ctx context.Context, p rpc.WPParams) (map[string]string, error) {
	steps, ok := updateSteps[p.What]
	if !ok {
		return nil, agentrpc.Invalid("unknown update target %q", p.What)
	}
	var errs []error
	for _, args := range steps {
		if _, err := o.Run(ctx, p.Site, args...); err != nil {
			errs = append(errs, fmt.Errorf("wp %s: %w", strings.Join(args, " "), err))
			// "all" runs every step (a plugin update does not need a core
			// update); a single target stops at its first failure.
			if p.What != "all" {
				break
			}
		}
	}
	// Old bytecode and cached options must not outlive the update, also after
	// a partial one.
	if err := o.Refresh(ctx, p.Site); err != nil {
		o.log().Warn("site caches not refreshed after an update", "site", p.Site.Domain, "err", err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return map[string]string{"updated": p.What}, nil
}

// SearchReplace replaces from with to in every table of the site's prefix,
// GUIDs excepted (SPEC §5).
func (o *Ops) SearchReplace(ctx context.Context, s state.Site, from, to string) error {
	if from == "" || to == "" || from == to || strings.HasPrefix(from, "-") || strings.HasPrefix(to, "-") {
		return agentrpc.Invalid("invalid search-replace values")
	}
	_, err := o.Run(ctx, s, "search-replace", from, to, "--all-tables-with-prefix", "--precise", "--skip-columns=guid")
	return err
}
