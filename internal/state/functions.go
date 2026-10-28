package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Standalone functions (#143 v0.9.0, internal/functions SPEC §1, api SPEC
// §17): a server-wide resource, one row each in functions, their own host
// names in function_domains. A secret's value is the sealed FunctionSecret,
// the bearer token FunctionTokenSecret.

// Function is one standalone function.
type Function struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Runtime     string            `json:"runtime"`
	Version     string            `json:"version"`
	MemoryMB    int               `json:"memory_mb"`
	TimeoutSec  int               `json:"timeout_sec"`
	Schedule    string            `json:"schedule"`
	Env         map[string]string `json:"env"`
	Secrets     []string          `json:"secrets"`
	Auth        string            `json:"auth"`
	CORSOrigins []string          `json:"cors_origins"`
	Domains     []string          `json:"domains"`
	Release     string            `json:"release"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
}

// A function's auth (internal/functions SPEC §1.1).
const (
	FunctionPublic = "public"
	FunctionToken  = "token"
)

// ErrFunctionNameTaken is a name another function has.
var ErrFunctionNameTaken = errors.New("a function of that name exists")

// ErrFunctionDomainTaken is a host name another function has.
var ErrFunctionDomainTaken = errors.New("another function has that domain")

// FunctionSecretPrefix starts every secret of one function.
func FunctionSecretPrefix(id int64) string { return "fn." + strconv.FormatInt(id, 10) + "." }

// FunctionSecret names a function's secret variable's value.
func FunctionSecret(id int64, name string) string { return FunctionSecretPrefix(id) + name }

// FunctionTokenSecret names a function's bearer token. A variable cannot be
// called "token": variable names are upper-case (internal/runtimes SPEC §2).
func FunctionTokenSecret(id int64) string { return FunctionSecretPrefix(id) + "token" }

const functionCols = `id, name, runtime, version, memory_mb, timeout_sec, schedule, env, secrets, auth, cors_origins, release, created_at, updated_at`

func scanFunction(row interface{ Scan(...any) error }) (Function, error) {
	var f Function
	var env, secrets, cors string
	if err := row.Scan(&f.ID, &f.Name, &f.Runtime, &f.Version, &f.MemoryMB, &f.TimeoutSec, &f.Schedule,
		&env, &secrets, &f.Auth, &cors, &f.Release, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return f, err
	}
	for _, c := range []struct {
		col string
		dst any
	}{{env, &f.Env}, {secrets, &f.Secrets}, {cors, &f.CORSOrigins}} {
		if err := json.Unmarshal([]byte(c.col), c.dst); err != nil {
			return f, fmt.Errorf("function %s: %w", f.Name, err)
		}
	}
	f.normalise()
	return f, nil
}

// normalise gives the maps and lists their empty values.
func (f *Function) normalise() {
	if f.Env == nil {
		f.Env = map[string]string{}
	}
	if f.Secrets == nil {
		f.Secrets = []string{}
	}
	if f.CORSOrigins == nil {
		f.CORSOrigins = []string{}
	}
	if f.Domains == nil {
		f.Domains = []string{}
	}
	if f.Auth == "" {
		f.Auth = FunctionPublic
	}
}

func functionJSON(f Function) (env, secrets, cors string, err error) {
	f.normalise()
	var b []byte
	if b, err = json.Marshal(f.Env); err != nil {
		return
	}
	env = string(b)
	if b, err = json.Marshal(f.Secrets); err != nil {
		return
	}
	secrets = string(b)
	if b, err = json.Marshal(f.CORSOrigins); err != nil {
		return
	}
	cors = string(b)
	return
}

func uniqueErr(err error, taken error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return taken
	}
	return err
}

// CreateFunction inserts a function (its domains are SetFunctionDomains')
// and returns it with its id.
func (s *Store) CreateFunction(f Function) (Function, error) {
	f.normalise()
	env, secrets, cors, err := functionJSON(f)
	if err != nil {
		return Function{}, err
	}
	t := now()
	err = s.db.QueryRow(`INSERT INTO functions (name, runtime, version, memory_mb, timeout_sec, schedule, env, secrets, auth, cors_origins, release, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		f.Name, f.Runtime, f.Version, f.MemoryMB, f.TimeoutSec, f.Schedule, env, secrets, f.Auth, cors, f.Release, t, t).Scan(&f.ID)
	if err != nil {
		return Function{}, uniqueErr(err, ErrFunctionNameTaken)
	}
	f.CreatedAt, f.UpdatedAt, f.Domains = t, t, []string{}
	return f, nil
}

// GetFunction reads one by name, with its domains; ErrNotFound when there
// is none.
func (s *Store) GetFunction(name string) (Function, error) {
	f, err := scanFunction(s.db.QueryRow(`SELECT `+functionCols+` FROM functions WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Function{}, ErrNotFound
	}
	if err != nil {
		return Function{}, err
	}
	f.Domains, err = s.domainsOf(s.db, f.ID)
	return f, err
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) domainsOf(q querier, id int64) ([]string, error) {
	rows, err := q.Query(`SELECT domain FROM function_domains WHERE function_id = ? ORDER BY domain`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListFunctions returns every function by name, with its domains.
func (s *Store) ListFunctions() ([]Function, error) {
	rows, err := s.db.Query(`SELECT ` + functionCols + ` FROM functions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	list := []Function{}
	for rows.Next() {
		f, err := scanFunction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	doms, err := s.functionDomainsByID()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if d := doms[list[i].ID]; d != nil {
			list[i].Domains = d
		}
	}
	return list, nil
}

func (s *Store) functionDomainsByID() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT function_id, domain FROM function_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var d string
		if err := rows.Scan(&id, &d); err != nil {
			return nil, err
		}
		out[id] = append(out[id], d)
	}
	return out, rows.Err()
}

// FunctionDomains maps every function's own host name to the function's
// name (the names check, api SPEC §3.10).
func (s *Store) FunctionDomains() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT d.domain, f.name FROM function_domains d JOIN functions f ON f.id = d.function_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var d, n string
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}

// UpdateFunction applies fn to the function as stored now and saves it in
// one write transaction (its id, name, created_at and domains stay). A
// non-nil error from fn aborts the change and is returned as is.
func (s *Store) UpdateFunction(name string, fn func(*Function) error) (Function, error) {
	var out Function
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		f, err := scanFunction(conn.QueryRowContext(ctx, `SELECT `+functionCols+` FROM functions WHERE name = ?`, name))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		id, created := f.ID, f.CreatedAt
		if err := fn(&f); err != nil {
			return err
		}
		f.ID, f.Name, f.CreatedAt = id, name, created
		f.UpdatedAt = now()
		env, secrets, cors, err := functionJSON(f)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE functions SET runtime=?, version=?, memory_mb=?, timeout_sec=?, schedule=?, env=?, secrets=?, auth=?, cors_origins=?, release=?, updated_at=? WHERE id=?`,
			f.Runtime, f.Version, f.MemoryMB, f.TimeoutSec, f.Schedule, env, secrets, f.Auth, cors, f.Release, f.UpdatedAt, f.ID); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `SELECT domain FROM function_domains WHERE function_id = ? ORDER BY domain`, f.ID)
		if err != nil {
			return err
		}
		f.Domains = []string{}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return err
			}
			f.Domains = append(f.Domains, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		f.normalise()
		out = f
		return nil
	})
	return out, err
}

// SetFunctionDomains replaces a function's own host names; ErrFunctionDomainTaken
// when another function has one.
func (s *Store) SetFunctionDomains(id int64, domains []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM function_domains WHERE function_id = ?`, id); err != nil {
		return err
	}
	for _, d := range domains {
		if _, err := tx.Exec(`INSERT INTO function_domains (function_id, domain) VALUES (?, ?)`, id, d); err != nil {
			return uniqueErr(err, ErrFunctionDomainTaken)
		}
	}
	return tx.Commit()
}

// DeleteFunction removes a function, its domains and every fn.<id>. secret
// in one transaction; ErrNotFound when there is none.
func (s *Store) DeleteFunction(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM functions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM function_domains WHERE function_id = ?`, id); err != nil {
		return err
	}
	if err := deleteSecretsWithPrefix(tx, FunctionSecretPrefix(id)); err != nil {
		return err
	}
	return tx.Commit()
}
