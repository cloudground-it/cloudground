package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cloudground-it/cloudground/internal/jobs"
	_ "modernc.org/sqlite"
)

// statePeek reads two answers out of the panel's state database without the
// API: the release channel it follows, and whether a job is running that an
// update or an uninstall would cut (internal/jobs SPEC §11). It opens the
// file read-only and never migrates it: the API owns its schema.
type statePeek struct{ db *sql.DB }

const defaultStateDB = "/var/lib/cloudground/state.db"

func openStatePeek(path string) (*statePeek, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &statePeek{db: db}, nil
}

func (p *statePeek) Close() error { return p.db.Close() }

// channel is the panel's update_channel setting.
func (p *statePeek) channel() (string, error) {
	var ch string
	err := p.db.QueryRow(`SELECT update_channel FROM panel_settings WHERE id = 1`).Scan(&ch)
	if errors.Is(err, sql.ErrNoRows) {
		return "stable", nil
	}
	return ch, err
}

// blockingJobs names every running job an update or an uninstall must wait
// for, as "task <id> (<kind>)".
func (p *statePeek) blockingJobs() ([]string, error) {
	rows, err := p.db.Query(`SELECT id, kind FROM jobs WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		if jobs.BlocksPanelChange(kind) {
			out = append(out, fmt.Sprintf("task %d (%s)", id, kind))
		}
	}
	return out, rows.Err()
}

// runningBlockers is blockingJobs of the state database at path, as one
// error naming them, nil when none runs. A database that does not exist has
// no job.
func runningBlockers(path string) error {
	p, err := openStatePeek(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read the job queue in %s: %w", path, err)
	}
	defer p.Close()
	names, err := p.blockingJobs()
	if err != nil {
		return fmt.Errorf("cannot read the job queue in %s: %w", path, err)
	}
	if len(names) > 0 {
		return fmt.Errorf("%s running: wait until it has finished", strings.Join(names, ", "))
	}
	return nil
}
