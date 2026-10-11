// Package rpc defines the documents panel-api and panel-agent exchange: for
// every method of internal/agentrpc's table, the JSON of its parameters and
// the JSON of its result. SPEC.md in this directory is the contract.
//
// The API is unprivileged and the agent is root, so nothing here is a
// command: a value names what to do (a site, a release, a table) and the
// agent decides how. The agent's server confines every site a value carries
// (Sites) and checks the value's own rules (Validate) before a handler runs.
//
// The JSON is fixed by the golden files in testdata/wire.
package rpc
