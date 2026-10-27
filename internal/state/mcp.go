package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The remote MCP server's records (#70; SPEC.md, Entities; the rules are
// internal/api/SPEC.md §16): the OAuth clients that registered, the grants a
// person consented to, their tokens stored as SHA-256 only, and the
// destructive actions waiting for a person to confirm them.

var (
	// ErrTooManyMCPClients refuses a registration past MaxMCPClients.
	ErrTooManyMCPClients = errors.New("too many MCP clients registered: revoke some from the panel")
	// ErrMCPRefreshReused is a refresh token presented after it was
	// rotated: someone copied it. The grant has been revoked.
	ErrMCPRefreshReused = errors.New("refresh token reused")
)

const (
	// MaxMCPClients is how many registered clients the panel keeps.
	MaxMCPClients = 200
	// mcpClientUnusedTTL: a client never granted anything is forgotten
	// after this long.
	mcpClientUnusedTTL = 24 * time.Hour
)

// MCPClient is a client that registered itself (RFC 7591).
type MCPClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// AddMCPClient stores a client. Clients never granted anything and older
// than a day are forgotten first; past MaxMCPClients it refuses
// (ErrTooManyMCPClients).
func (s *Store) AddMCPClient(c MCPClient) (MCPClient, error) {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return MCPClient{}, err
	}
	at := time.Now().UTC()
	err = s.inTx(func(tx *sql.Tx) error {
		cutoff := mcpStamp(at.Add(-mcpClientUnusedTTL))
		if _, err := tx.Exec(`DELETE FROM mcp_clients WHERE created_at < ?
			AND NOT EXISTS (SELECT 1 FROM mcp_grants g WHERE g.client_id = mcp_clients.id)`, cutoff); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM mcp_clients`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxMCPClients {
			return ErrTooManyMCPClients
		}
		_, err := tx.Exec(`INSERT INTO mcp_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
			c.ID, c.Name, string(uris), mcpStamp(at))
		return err
	})
	if err != nil {
		return MCPClient{}, err
	}
	c.CreatedAt = at
	return c, nil
}

// GetMCPClient returns a registered client.
func (s *Store) GetMCPClient(id string) (MCPClient, error) {
	var c MCPClient
	var uris, created string
	err := s.db.QueryRow(`SELECT id, name, redirect_uris, created_at FROM mcp_clients WHERE id = ?`, id).Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPClient{}, ErrNotFound
	}
	if err != nil {
		return MCPClient{}, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return MCPClient{}, err
	}
	c.CreatedAt = parseTime(created)
	return c, nil
}

// MCPGrant is what a person consented to for one client: the scopes (JSON,
// the api package's), and when its tokens expire.
type MCPGrant struct {
	ID             int64
	UserID         int64
	Email          string
	ClientID       string
	ClientName     string
	Scopes         string
	AccessExpires  time.Time
	RefreshExpires time.Time
	CreatedAt      time.Time
	LastUsedAt     time.Time
}

// MCPTokens are the hashes of a grant's current tokens and their expiries.
type MCPTokens struct {
	AccessHash, RefreshHash       string
	AccessExpires, RefreshExpires time.Time
}

const mcpGrantCols = `g.id, g.user_id, u.email, g.client_id, c.name, g.scopes, g.access_expires, g.refresh_expires, g.created_at, g.last_used_at`
const mcpGrantFrom = ` FROM mcp_grants g JOIN users u ON u.id = g.user_id JOIN mcp_clients c ON c.id = g.client_id `

func scanMCPGrant(row interface{ Scan(...any) error }) (MCPGrant, error) {
	var g MCPGrant
	var ae, re, created, used string
	if err := row.Scan(&g.ID, &g.UserID, &g.Email, &g.ClientID, &g.ClientName, &g.Scopes, &ae, &re, &created, &used); err != nil {
		return MCPGrant{}, err
	}
	g.AccessExpires, g.RefreshExpires = parseTime(ae), parseTime(re)
	g.CreatedAt, g.LastUsedAt = parseTime(created), parseTime(used)
	return g, nil
}

// mcpStamp is a fixed-width UTC time, so stored times compare as text.
func mcpStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// CreateMCPGrant records a consent with its first tokens.
func (s *Store) CreateMCPGrant(userID int64, clientID, scopes string, t MCPTokens) (MCPGrant, error) {
	res, err := s.db.Exec(`INSERT INTO mcp_grants (user_id, client_id, scopes, access_hash, access_expires, refresh_hash, refresh_expires, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, clientID, scopes, t.AccessHash, mcpStamp(t.AccessExpires), t.RefreshHash, mcpStamp(t.RefreshExpires), mcpStamp(time.Now()))
	if err != nil {
		return MCPGrant{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MCPGrant{}, err
	}
	_, _ = s.db.Exec(`UPDATE mcp_clients SET last_used_at = ? WHERE id = ?`, now(), clientID)
	return s.GetMCPGrant(id)
}

// GetMCPGrant returns a grant by id.
func (s *Store) GetMCPGrant(id int64) (MCPGrant, error) {
	g, err := scanMCPGrant(s.db.QueryRow(`SELECT `+mcpGrantCols+mcpGrantFrom+`WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPGrant{}, ErrNotFound
	}
	return g, err
}

// MCPGrantByAccess returns the grant whose current access token hashes to
// hash, if that token has not expired at now; it records the use (at most
// once a minute).
func (s *Store) MCPGrantByAccess(hash string, at time.Time) (MCPGrant, error) {
	g, err := scanMCPGrant(s.db.QueryRow(`SELECT `+mcpGrantCols+mcpGrantFrom+`WHERE g.access_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPGrant{}, ErrNotFound
	}
	if err != nil {
		return MCPGrant{}, err
	}
	if !at.Before(g.AccessExpires) {
		return MCPGrant{}, ErrNotFound
	}
	if at.Sub(g.LastUsedAt) > time.Minute {
		_, _ = s.db.Exec(`UPDATE mcp_grants SET last_used_at = ? WHERE id = ?`, mcpStamp(at), g.ID)
	}
	return g, nil
}

// RotateMCPGrant exchanges a grant's current refresh token, of clientID and
// unexpired at now, for new tokens, keeping the old one's hash as the one
// rotated out. A refresh token already rotated out revokes its grant and
// is ErrMCPRefreshReused (with the grant, for the audit); any other is
// ErrNotFound.
func (s *Store) RotateMCPGrant(refreshHash, clientID string, at time.Time, next MCPTokens) (MCPGrant, error) {
	var id int64
	var reused bool
	err := s.inTx(func(tx *sql.Tx) error {
		var expires string
		err := tx.QueryRow(`SELECT id, refresh_expires FROM mcp_grants WHERE refresh_hash = ? AND client_id = ?`, refreshHash, clientID).Scan(&id, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRow(`SELECT id FROM mcp_grants WHERE prev_refresh_hash = ? AND prev_refresh_hash <> ''`, refreshHash).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			reused = true
			return nil
		}
		if err != nil {
			return err
		}
		if !at.Before(parseTime(expires)) {
			return ErrNotFound
		}
		_, err = tx.Exec(`UPDATE mcp_grants SET prev_refresh_hash = refresh_hash, refresh_hash = ?, refresh_expires = ?,
			access_hash = ?, access_expires = ?, last_used_at = ? WHERE id = ?`,
			next.RefreshHash, mcpStamp(next.RefreshExpires), next.AccessHash, mcpStamp(next.AccessExpires), mcpStamp(at), id)
		return err
	})
	if err != nil {
		return MCPGrant{}, err
	}
	g, err := s.GetMCPGrant(id)
	if reused {
		if _, derr := s.db.Exec(`DELETE FROM mcp_grants WHERE id = ?`, id); derr != nil {
			return g, derr
		}
		return g, ErrMCPRefreshReused
	}
	return g, err
}

// RevokeMCPGrantByToken revokes the grant whose access or refresh token
// hashes to hash, and returns it; false when there is none.
func (s *Store) RevokeMCPGrantByToken(hash string) (MCPGrant, bool, error) {
	g, err := scanMCPGrant(s.db.QueryRow(`SELECT `+mcpGrantCols+mcpGrantFrom+`WHERE g.access_hash = ? OR g.refresh_hash = ?`, hash, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPGrant{}, false, nil
	}
	if err != nil {
		return MCPGrant{}, false, err
	}
	_, err = s.db.Exec(`DELETE FROM mcp_grants WHERE id = ?`, g.ID)
	return g, err == nil, err
}

// DeleteMCPGrant revokes a grant and returns what it was.
func (s *Store) DeleteMCPGrant(id int64) (MCPGrant, error) {
	g, err := s.GetMCPGrant(id)
	if err != nil {
		return MCPGrant{}, err
	}
	_, err = s.db.Exec(`DELETE FROM mcp_grants WHERE id = ?`, id)
	return g, err
}

// ListMCPGrants returns the grants of userID (0: every account's), newest
// first.
func (s *Store) ListMCPGrants(userID int64) ([]MCPGrant, error) {
	q := `SELECT ` + mcpGrantCols + mcpGrantFrom
	var args []any
	if userID != 0 {
		q += `WHERE g.user_id = ? `
		args = append(args, userID)
	}
	rows, err := s.db.Query(q+`ORDER BY g.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPGrant
	for rows.Next() {
		g, err := scanMCPGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MCPAction is a destructive tool call waiting for a person.
type MCPAction struct {
	ID         int64
	GrantID    int64
	UserID     int64
	ClientName string
	Tool       string
	Args       string // JSON
	Summary    string
	Status     string
	Result     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DecidedAt  time.Time
}

const mcpActionCols = `a.id, a.grant_id, a.user_id, c.name, a.tool, a.args, a.summary, a.status, a.result, a.created_at, a.expires_at, a.decided_at`
const mcpActionFrom = ` FROM mcp_actions a JOIN mcp_grants g ON g.id = a.grant_id JOIN mcp_clients c ON c.id = g.client_id `

func scanMCPAction(row interface{ Scan(...any) error }, at time.Time) (MCPAction, error) {
	var a MCPAction
	var created, expires, decided string
	if err := row.Scan(&a.ID, &a.GrantID, &a.UserID, &a.ClientName, &a.Tool, &a.Args, &a.Summary, &a.Status, &a.Result, &created, &expires, &decided); err != nil {
		return MCPAction{}, err
	}
	a.CreatedAt, a.ExpiresAt, a.DecidedAt = parseTime(created), parseTime(expires), parseTime(decided)
	// A pending action past its expiry reads as expired.
	if a.Status == "pending" && !at.Before(a.ExpiresAt) {
		a.Status = "expired"
	}
	return a, nil
}

// AddMCPAction records a pending action.
func (s *Store) AddMCPAction(a MCPAction) (MCPAction, error) {
	res, err := s.db.Exec(`INSERT INTO mcp_actions (grant_id, user_id, tool, args, summary, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.GrantID, a.UserID, a.Tool, a.Args, a.Summary, mcpStamp(a.CreatedAt), mcpStamp(a.ExpiresAt))
	if err != nil {
		return MCPAction{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MCPAction{}, err
	}
	return s.GetMCPAction(id, a.CreatedAt)
}

// GetMCPAction returns an action as it stands at at.
func (s *Store) GetMCPAction(id int64, at time.Time) (MCPAction, error) {
	a, err := scanMCPAction(s.db.QueryRow(`SELECT `+mcpActionCols+mcpActionFrom+`WHERE a.id = ?`, id), at)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPAction{}, ErrNotFound
	}
	return a, err
}

// ListMCPActions returns the account's actions, newest first, at most
// limit.
func (s *Store) ListMCPActions(userID int64, limit int, at time.Time) ([]MCPAction, error) {
	rows, err := s.db.Query(`SELECT `+mcpActionCols+mcpActionFrom+`WHERE a.user_id = ? ORDER BY a.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPAction
	for rows.Next() {
		a, err := scanMCPAction(rows, at)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideMCPAction moves a pending action, unexpired at at, to status with
// result, and reports whether it was still pending: of two decisions at
// once, one wins.
func (s *Store) DecideMCPAction(id int64, at time.Time, status, result string) (bool, error) {
	res, err := s.db.Exec(`UPDATE mcp_actions SET status = ?, result = ?, decided_at = ? WHERE id = ? AND status = 'pending' AND expires_at > ?`,
		status, result, mcpStamp(at), id, mcpStamp(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishMCPAction records the outcome of a confirmed action.
func (s *Store) FinishMCPAction(id int64, status, result string) error {
	_, err := s.db.Exec(`UPDATE mcp_actions SET status = ?, result = ? WHERE id = ?`, status, result, id)
	return err
}

// ClearMCP deletes every client, grant and action, in one transaction:
// turning the MCP server off (api SPEC §16.1).
func (s *Store) ClearMCP() error {
	return s.inTx(func(tx *sql.Tx) error {
		for _, t := range []string{"mcp_actions", "mcp_grants", "mcp_clients"} {
			if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
				return err
			}
		}
		return nil
	})
}
