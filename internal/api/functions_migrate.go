package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The migration of v0.8.x site functions (SPEC.md §17.8): each becomes a
// standalone function with its code, releases, variables and secrets; its
// old address on the site stops answering.

const fnMigrationDone = "done"

// queueFunctionsMigration queues functions.migrate when a site still holds
// a v0.8.x function and the migration has not completed.
func (s *Server) queueFunctionsMigration() {
	ps, err := s.Store.PanelSettings()
	if err != nil || ps.FunctionsMigration == fnMigrationDone {
		return
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		return
	}
	var ids []int64
	for _, st := range sites {
		if len(st.Config.Functions) > 0 {
			ids = append(ids, st.ID)
		}
	}
	if len(ids) == 0 {
		_, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
			p.FunctionsMigration = fnMigrationDone
			return nil
		})
		s.logIfErr(err, "UpdatePanelSettings")
		return
	}
	if _, err := s.startJob(jobReq{kind: "functions.migrate", locks: siteLocks(ids...), dedupKey: "functions.migrate"},
		func(ctx context.Context, _ state.Site, progress func(int, string)) error {
			return s.migrateFunctions(ctx, progress)
		}); err != nil {
		s.Log.Warn("could not queue the functions migration", "err", err)
	}
}

// migrateFunctions moves every v0.8.x function out of its site, then
// renders the functions and records the outcome.
func (s *Server) migrateFunctions(ctx context.Context, progress func(int, string)) error {
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	var errs []string
	moved := 0
	for _, st := range sites {
		for _, old := range st.Config.Functions {
			name, err := s.migrateFunction(ctx, st, old)
			if err != nil {
				s.Log.Warn("a v0.8.x function could not be moved", "site", st.Domain, "function", old.Name, "err", err)
				errs = append(errs, st.Domain+"/"+old.Name+": "+err.Error())
				progress(-1, "Could not move "+st.Domain+"/"+old.Name+": "+err.Error())
				continue
			}
			moved++
			progress(-1, "Moved "+st.Domain+"/"+old.Name+" to the function "+name)
		}
	}
	if err := s.renderFunctions(ctx, renderWith{}); err != nil {
		errs = append(errs, "render: "+err.Error())
	}
	marker := fnMigrationDone
	if len(errs) > 0 {
		marker = "failed: " + errs[0]
		if len(marker) > 500 {
			marker = marker[:500]
		}
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.FunctionsMigration = marker
		return nil
	}); err != nil {
		return err
	}
	s.kickAlerts()
	progress(100, fmt.Sprintf("%d functions moved out of their sites", moved))
	if len(errs) > 0 {
		return errors.New(marker)
	}
	return nil
}

// migratedName is the name a site's function takes: its own, or, taken,
// <name>-<site id> cut to 31 characters.
func (s *Server) migratedName(old string, siteID int64) (string, error) {
	free := func(n string) bool {
		if slices.Contains(reservedFunctionNames, n) {
			return false
		}
		_, err := s.Store.GetFunction(n)
		return errors.Is(err, state.ErrNotFound)
	}
	if free(old) {
		return old, nil
	}
	suffix := fmt.Sprintf("-%d", siteID)
	base := old
	if len(base)+len(suffix) > 31 {
		base = strings.TrimRight(base[:31-len(suffix)], "-")
	}
	if n := base + suffix; functions.ValidName(n) && free(n) {
		return n, nil
	}
	return "", fmt.Errorf("no free name for %s", old)
}

// migrateFunction moves one site function (§17.8 steps 1–5).
func (s *Server) migrateFunction(ctx context.Context, st state.Site, old state.SiteFunction) (string, error) {
	name, err := s.migratedName(old.Name, st.ID)
	if err != nil {
		return "", err
	}
	f := functions.Defaults(state.Function{
		Name: name, Runtime: old.Runtime, Version: old.Version, MemoryMB: old.MemoryMB, TimeoutSec: old.TimeoutSec,
		Schedule: old.Schedule, Env: old.Env, Secrets: slices.Clone(old.Secrets), Auth: state.FunctionPublic,
	})
	if err := functions.Check(f); err != nil {
		return "", err
	}
	created, err := s.Store.CreateFunction(f)
	if err != nil {
		return "", err
	}
	f.ID = created.ID
	undo := func() { s.logIfErr(s.Store.DeleteFunction(f.ID), "DeleteFunction") }
	env := map[string]string{}
	for k, v := range old.Env {
		env[k] = v
	}
	oldSecret := func(n string) string { return state.SiteSecret(st.ID, "fn."+old.Name+"."+n) }
	for _, n := range old.Secrets {
		v, err := s.Store.Secret(oldSecret(n))
		if err == nil {
			err = s.Store.SetSecret(state.FunctionSecret(f.ID, n), v)
		}
		if err != nil {
			undo()
			return "", err
		}
		env[n] = v
	}
	cur, unlock, err := s.freshSite(st.ID)
	if err != nil {
		unlock()
		undo()
		return "", err
	}
	defer unlock()
	var res rpc.FunctionResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.MigrateFunction, rpc.MigrateFunctionParams{Site: cur, OldName: old.Name, Function: f, Env: env}, &res); err != nil {
		undo()
		return "", err
	}
	if _, err := s.Store.UpdateFunction(name, func(sf *state.Function) error {
		sf.Release = res.Release
		return nil
	}); err != nil {
		return name, err
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(site *state.Site) error {
		site.Config.Functions = slices.DeleteFunc(slices.Clone(site.Config.Functions), func(x state.SiteFunction) bool { return x.Name == old.Name })
		if len(site.Config.Functions) == 0 {
			site.Config.Functions = nil
		}
		return nil
	})
	if err != nil {
		return name, err
	}
	// The site without its /.functions/ locations: the engine renders none
	// any more, so any render takes them away; this one does it now.
	var applied rpc.ApplyResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: stored, ReloadOnlyIfChanged: true}, &applied); err != nil {
		s.Log.Warn("a site could not be rendered without its moved function", "site", st.Domain, "err", err)
	} else {
		s.recordRendered(stored, applied.Files)
	}
	for _, n := range old.Secrets {
		s.logIfErr(s.Store.SetSecret(oldSecret(n), ""), "SetSecret")
	}
	s.audit("system", "function.migrate", name, st.Domain+"/"+old.Name)
	return name, nil
}

// functionsConditions is the functions alert (§11.1): a migration that
// could not move everything.
func functionsConditions(ps state.PanelSettings) []state.Alert {
	detail, failed := strings.CutPrefix(ps.FunctionsMigration, "failed: ")
	if !failed {
		return nil
	}
	return []state.Alert{{Key: "functions.migrate", Level: "error", Fingerprint: detail,
		Message: "some v0.8.x functions could not be moved: " + detail,
		Text:    state.AlertText{Key: "alert.functions_migrate", Args: []string{detail}}}}
}
