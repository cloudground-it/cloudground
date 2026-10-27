package rpc

import (
	"errors"
	"fmt"

	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's functions (#143, ops/functions SPEC).

// SetFunctionParams sets a function up: its settings, a new release of its
// code when Code is set, and its whole environment.
type SetFunctionParams struct {
	Site     state.Site         `json:"site"`
	Function state.SiteFunction `json:"function"`
	Code     string             `json:"code,omitempty"`
	Env      map[string]string  `json:"env"`
}

func (p *SetFunctionParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the function, the code and the environment (SPEC §3).
func (p *SetFunctionParams) Validate() error {
	if err := functions.Check(p.Function); err != nil {
		return err
	}
	if err := functions.CheckCode(p.Code); err != nil {
		return err
	}
	return runtimes.CheckEnv(p.Env)
}

// FunctionRef names one function of a site.
type FunctionRef struct {
	Site state.Site `json:"site"`
	Name string     `json:"name"`
}

func (p *FunctionRef) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the name.
func (p *FunctionRef) Validate() error {
	if !functions.ValidName(p.Name) {
		return fmt.Errorf("invalid function name %q", p.Name)
	}
	return nil
}

// FunctionRollbackParams moves a function's current release.
type FunctionRollbackParams struct {
	Site    state.Site `json:"site"`
	Name    string     `json:"name"`
	Release string     `json:"release,omitempty"`
}

func (p *FunctionRollbackParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the name and the release.
func (p *FunctionRollbackParams) Validate() error {
	if !functions.ValidName(p.Name) {
		return fmt.Errorf("invalid function name %q", p.Name)
	}
	if p.Release != "" && !site.ValidReleaseID(p.Release) {
		return fmt.Errorf("invalid release id %q", p.Release)
	}
	return nil
}

// FunctionResult is the live release and every release, newest first.
type FunctionResult struct {
	Release  string   `json:"release"`
	Releases []string `json:"releases"`
}

// FunctionCodeResult is the live code.
type FunctionCodeResult struct {
	Release  string   `json:"release"`
	Code     string   `json:"code"`
	Releases []string `json:"releases"`
}

// InvokeFunctionParams runs a function once through nginx.
type InvokeFunctionParams struct {
	Site       state.Site `json:"site"`
	Name       string     `json:"name"`
	Source     string     `json:"source"`
	Body       string     `json:"body,omitempty"`
	TimeoutSec int        `json:"timeout_sec"`
}

func (p *InvokeFunctionParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the name, the source and the body.
func (p *InvokeFunctionParams) Validate() error {
	if !functions.ValidName(p.Name) {
		return fmt.Errorf("invalid function name %q", p.Name)
	}
	if p.Source != "schedule" && p.Source != "test" {
		return errors.New("the source is schedule or test")
	}
	if len(p.Body) > 64<<10 {
		return errors.New("a test body is at most 64 KiB")
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

// Invocation is one request to a function, from the access log.
type Invocation struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
	Source string `json:"source"`
}

// FunctionLogsResult is a function's recent invocations and output.
type FunctionLogsResult struct {
	Invocations []Invocation `json:"invocations"`
	Output      []string     `json:"output"`
}
