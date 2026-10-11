// Package state holds what CloudGround wants every site and the panel to
// look like, in one SQLite database. The agent renders the system's
// configuration from these records; nothing here is read back from the
// system except the observations SPEC.md names. The API is its only user.
//
// The types that cross the wire (to the browser as JSON, to the agent over
// rpc) live in model_*.go, one file per domain; their field names, order
// and JSON tags are a contract.
package state
