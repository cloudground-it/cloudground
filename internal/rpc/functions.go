package rpc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Standalone functions (#143 v0.9.0, ops/functions SPEC).

// SetFunctionParams sets a function up: its settings, a new release of its
// code when Files is set, and its whole environment.
type SetFunctionParams struct {
	Function state.Function    `json:"function"`
	Files    map[string]string `json:"files,omitempty"`
	Env      map[string]string `json:"env"`
}

// Validate checks the function, the files and the environment (SPEC §3).
func (p *SetFunctionParams) Validate() error {
	return checkFunction(p.Function, p.Files, p.Env)
}

func checkFunction(f state.Function, files, env map[string]string) error {
	if f.ID <= 0 {
		return errors.New("invalid function id")
	}
	if err := functions.Check(f); err != nil {
		return err
	}
	if files != nil {
		if err := functions.CheckFiles(f.Runtime, files); err != nil {
			return err
		}
	}
	return runtimes.CheckEnv(env)
}

// FunctionRef names one function.
type FunctionRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Validate checks the id and the name.
func (p *FunctionRef) Validate() error { return checkRef(p.ID, p.Name) }

func checkRef(id int64, name string) error {
	if id <= 0 {
		return errors.New("invalid function id")
	}
	if !functions.ValidName(name) {
		return fmt.Errorf("invalid function name %q", name)
	}
	return nil
}

// FunctionRollbackParams moves a function's current release.
type FunctionRollbackParams struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Release string `json:"release,omitempty"`
}

// Validate checks the id, the name and the release.
func (p *FunctionRollbackParams) Validate() error {
	if err := checkRef(p.ID, p.Name); err != nil {
		return err
	}
	if p.Release != "" && !site.ValidReleaseID(p.Release) {
		return fmt.Errorf("invalid release id %q", p.Release)
	}
	return nil
}

// FunctionResult is the live release, every release (newest first) and
// the dependency step's output.
type FunctionResult struct {
	Release  string   `json:"release"`
	Releases []string `json:"releases"`
	Output   string   `json:"output,omitempty"`
}

// FunctionCodeResult is the live code.
type FunctionCodeResult struct {
	Release  string            `json:"release"`
	Files    map[string]string `json:"files"`
	Releases []string          `json:"releases"`
}

// InvokeFunctionParams runs a function once through nginx.
type InvokeFunctionParams struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	Body       string `json:"body,omitempty"`
	TimeoutSec int    `json:"timeout_sec"`
}

// InvokeMethods are the methods a test run may use.
var InvokeMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// Validate checks the name, the source, the method, the path and the body.
func (p *InvokeFunctionParams) Validate() error {
	if !functions.ValidName(p.Name) {
		return fmt.Errorf("invalid function name %q", p.Name)
	}
	if p.Source != "schedule" && p.Source != "test" {
		return errors.New("the source is schedule or test")
	}
	if p.Method != "" && !slices.Contains(InvokeMethods, p.Method) {
		return errors.New("the method is GET, POST, PUT, PATCH or DELETE")
	}
	if err := CheckInvokePath(p.Path); err != nil {
		return err
	}
	if len(p.Body) > 64<<10 {
		return errors.New("a test body is at most 64 KiB")
	}
	return nil
}

// CheckInvokePath refuses a test run's path: "" or starting with /, at
// most 2 KiB, no space or control character.
func CheckInvokePath(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") || len(p) > 2<<10 {
		return errors.New("the path starts with / and is at most 2 KiB")
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f {
			return errors.New("the path has a space or a control character")
		}
	}
	return nil
}

// InvokeResult is how one invocation went.
type InvokeResult struct {
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
	Body   string `json:"body"`
	Error  string `json:"error,omitempty"`
}

// Invocation is one request to a function, from its access log.
type Invocation struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
	Source string `json:"source"`
}

// FunctionLogsResult is a function's recent invocations and output.
type FunctionLogsResult struct {
	Invocations []Invocation `json:"invocations"`
	Output      []string     `json:"output"`
}

// FunctionsSummaryParams names the functions whose newest invocation is
// wanted.
type FunctionsSummaryParams struct {
	Names []string `json:"names"`
}

// Validate checks the names.
func (p *FunctionsSummaryParams) Validate() error {
	if len(p.Names) > functions.MaxFunctions {
		return errors.New("too many functions")
	}
	for _, n := range p.Names {
		if !functions.ValidName(n) {
			return fmt.Errorf("invalid function name %q", n)
		}
	}
	return nil
}

// FunctionsSummaryResult is each function's newest invocation.
type FunctionsSummaryResult struct {
	Last map[string]Invocation `json:"last"`
}

// FunctionRoute is one function as nginx routes to it (internal/engine
// SPEC §8).
type FunctionRoute struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	TimeoutSec  int      `json:"timeout_sec"`
	Token       string   `json:"token,omitempty"`
	CORSOrigins []string `json:"cors_origins,omitempty"`
	Domains     []string `json:"domains,omitempty"`
}

// RenderFunctionsParams is every function and the functions domain.
type RenderFunctionsParams struct {
	Domain    string          `json:"domain"`
	Functions []FunctionRoute `json:"functions"`
}

// Validate checks every route (internal/rpc SPEC §3).
func (p *RenderFunctionsParams) Validate() error {
	if p.Domain != "" {
		if err := site.ValidateDomain(p.Domain); err != nil {
			return err
		}
	}
	if len(p.Functions) > functions.MaxFunctions {
		return errors.New("too many functions")
	}
	for _, f := range p.Functions {
		if err := checkRef(f.ID, f.Name); err != nil {
			return err
		}
		if f.TimeoutSec < 1 || f.TimeoutSec > functions.MaxTimeout {
			return fmt.Errorf("function %s: invalid timeout", f.Name)
		}
		if f.Token != "" && !functions.ValidToken(f.Token) {
			return fmt.Errorf("function %s: invalid token", f.Name)
		}
		if err := functions.CheckOrigins(f.CORSOrigins); err != nil {
			return fmt.Errorf("function %s: %w", f.Name, err)
		}
		if len(f.Domains) > functions.MaxDomains {
			return fmt.Errorf("function %s: too many domains", f.Name)
		}
		for _, d := range f.Domains {
			if err := site.ValidateDomain(d); err != nil {
				return fmt.Errorf("function %s: %w", f.Name, err)
			}
		}
	}
	return nil
}

// RenderFunctionsResult is the files written, for drift.
type RenderFunctionsResult struct {
	Files []ManagedFile `json:"files"`
}

// FunctionsCertParams issues or deletes a functions lineage.
type FunctionsCertParams struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains,omitempty"`
	Email   string   `json:"email,omitempty"`
}

// Validate checks the lineage and the domains.
func (p *FunctionsCertParams) Validate() error {
	if !functions.ValidLineage(p.Name) {
		return fmt.Errorf("invalid certificate name %q", p.Name)
	}
	for _, d := range p.Domains {
		if err := site.ValidateDomain(d); err != nil {
			return err
		}
	}
	return nil
}

// MigrateFunctionParams makes a v0.8.x site function a standalone one.
type MigrateFunctionParams struct {
	Site     state.Site        `json:"site"`
	OldName  string            `json:"old_name"`
	Function state.Function    `json:"function"`
	Env      map[string]string `json:"env"`
}

func (p *MigrateFunctionParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the old name, the function and the environment.
func (p *MigrateFunctionParams) Validate() error {
	if !functions.ValidName(p.OldName) {
		return fmt.Errorf("invalid function name %q", p.OldName)
	}
	return checkFunction(p.Function, nil, p.Env)
}
