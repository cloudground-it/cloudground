package functions

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
)

// Migrate is MigrateFunction (SPEC §9): a v0.8.x site function becomes the
// standalone one; until the old one is removed, a failure undoes the new.
func (o *Ops) Migrate(ctx context.Context, p rpc.MigrateFunctionParams) (res rpc.FunctionResult, err error) {
	f, s := p.Function, p.Site
	if err := p.Validate(); err != nil {
		return res, agentrpc.Invalid("%s", err.Error())
	}
	if !site.ValidSystemUser(s.SystemUser) {
		return res, agentrpc.Invalid("invalid system user %q", s.SystemUser)
	}
	if o.Installed != nil && !o.Installed(f.Runtime, f.Version) {
		return res, agentrpc.Invalid("%s %s is not installed on this server", runtimes.Label(f.Runtime), f.Version)
	}
	src, err := o.OpenSite(s)
	if err != nil {
		return res, err
	}
	defer src.Close()
	old := fn.LegacyCodeDir(p.OldName)
	olds, err := src.Sub(old)
	if err != nil {
		return res, agentrpc.NotFound("the site's function %s has no code: %v", p.OldName, err)
	}
	defer olds.Close()
	oldLive, err := live(olds)
	if err != nil {
		return res, agentrpc.NotFound("the site's function %s has no code: %v", p.OldName, err)
	}
	// A function that has code already is another's: never undone here.
	if r, err := o.openExisting(f.Name); err == nil {
		_, lerr := live(r)
		r.Close()
		if lerr == nil {
			return res, agentrpc.Conflict("function %s already has code", f.Name)
		}
	}

	err = func() error {
		unlock := o.lock(f.Name)
		defer unlock()
		owner, err := o.ensureUser(ctx, f.ID)
		if err != nil {
			return err
		}
		r, err := o.openTree(f.Name, owner)
		if err != nil {
			return err
		}
		defer r.Close()
		for _, id := range releases(olds) {
			if err := sitefs.CopyTree(olds, "releases/"+id, r, "releases/"+id); err != nil {
				return fmt.Errorf("copy release %s: %w", id, err)
			}
		}
		if err := rootOwned(r).Symlink("releases/"+oldLive, "current"); err != nil {
			return err
		}
		if _, err := live(r); err != nil {
			return err
		}
		if err := o.install(ctx, f, p.Env); err != nil {
			return err
		}
		res, err = result(r)
		return err
	}()
	if err != nil {
		if derr := o.Delete(context.WithoutCancel(ctx), f.ID, f.Name); derr != nil {
			o.log().Warn("could not undo a failed function migration", "function", f.Name, "err", derr)
		}
		return rpc.FunctionResult{}, err
	}

	// The old function: units, environment and code.
	base := fn.LegacyUnitBase(s.SystemUser, p.OldName)
	errs := []error{o.removeUnitFiles(ctx, base+".socket", base+"@.service", base+"@*.service",
		filepath.Join(o.dir(), filepath.Base(fn.LegacyEnvFile(s.SystemUser, p.OldName))))}
	errs = append(errs, rootOwned(src).RemoveAll(old))
	if err := errors.Join(errs...); err != nil {
		o.log().Warn("the migrated function's old units or code could not all be removed", "site", s.Domain, "function", p.OldName, "err", err)
	}
	return res, nil
}
