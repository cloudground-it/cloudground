package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	moveFrom = "/var/backups/cloudground/a"
	moveTo   = "/var/backups/cloudground/b"
	repoIDA  = "aaaaaaaa00000000000000000000000000000000000000000000000000000000"
	repoIDB  = "bbbbbbbb00000000000000000000000000000000000000000000000000000000"
)

// moveAgent answers RepositoryInfo for the repository each call names:
// ids gives each one's ID, missing what it lacks; one not in ids does not
// exist.
func moveAgent(agent *fakeAgent, ids map[string]string, missing map[string][]string) {
	agent.setHook(func(method string) {
		if method != agentrpc.RepositoryInfo.Name() {
			return
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		p, _ := agent.lastParams[method].(rpc.RepositoryInfoParams)
		id, ok := ids[p.Repository]
		if !ok {
			agent.errs[method] = agentrpc.NotFound("the backup repository does not exist")
			return
		}
		delete(agent.errs, method)
		agent.results[method] = rpc.RepositoryInfoResult{ID: id, Snapshots: 3, Missing: append([]string{}, missing[p.Repository]...)}
	})
}

// moveSetup is a panel with backups in moveFrom: two sites, three records.
func moveSetup(t *testing.T) (*Server, *fakeAgent, *client, []state.Backup) {
	t.Helper()
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	setBackupRepo(t, s, moveFrom, "old-password")
	one := seedActiveSite(t, s, "one.example.com", state.SiteStatic, "", 0)
	two := seedActiveSite(t, s, "two.example.com", state.SiteStatic, "", 0)
	records := []state.Backup{
		backupRecord(t, s, one, moveFrom, "11111111aaaaaaaa", state.VerifyPassed),
		backupRecord(t, s, one, moveFrom, "22222222bbbbbbbb", state.VerifyPassed),
		backupRecord(t, s, two, moveFrom, "33333333cccccccc", state.VerifyPassed),
	}
	return s, agent, c, records
}

func repositoryOf(t *testing.T, s *Server, ids ...int64) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		b, err := s.Store.GetBackup(id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b.Repository)
	}
	return out
}

// §5.9: the refusals, before anything is queued.
func TestMoveRepositoryRefusals(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	move := func(body map[string]any) (int, string) {
		resp, b := c.do("POST", "/api/backups/repository/move", body)
		return resp.StatusCode, string(b)
	}
	if code, body := move(map[string]any{"mode": "copy", "repository": moveTo}); code != http.StatusPreconditionFailed {
		t.Fatalf("move with backups off = %d %s, want 412", code, body)
	}
	setBackupRepo(t, s, moveFrom, "old-password")
	for _, tc := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"mode": "rsync", "repository": moveTo}, "mode must be copy or moved"},
		{map[string]any{"mode": "copy"}, "give the new repository"},
		{map[string]any{"mode": "copy", "repository": "/etc/backups"}, "/var/backups/cloudground"},
		{map[string]any{"mode": "moved", "repository": moveFrom}, "this is already the backup repository"},
		{map[string]any{"mode": "copy", "repository": moveTo, "surprise": 1}, "malformed request"},
	} {
		code, body := move(tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("move %v = %d %s, want 400 %q", tc.body, code, body, tc.want)
		}
	}
	if agent.called(agentrpc.RepositoryInfo.Name()) || agent.called(agentrpc.CopyRepository.Name()) {
		t.Fatal("a refused move reached the agent")
	}
}

// §5.9 copy: the panel copies, checks every record arrived, then points the
// setting and every record at the new repository in one step, and
// restore-tests each site's newest backup there.
func TestMoveRepositoryCopy(t *testing.T) {
	s, agent, c, records := moveSetup(t)
	moveAgent(agent, map[string]string{moveFrom: repoIDA, moveTo: repoIDB}, nil)
	resp, body := c.do("POST", "/api/backups/repository/move", map[string]any{"mode": "copy", "repository": moveTo, "password": "new-password"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("move = %d %s", resp.StatusCode, body)
	}
	if j := waitTaskEnd(t, s, taskOf(t, body).ID); j.Status != state.JobSucceeded {
		t.Fatalf("move job = %s %q", j.Status, j.Error)
	}
	cp, _ := agent.lastParams[agentrpc.CopyRepository.Name()].(rpc.CopyRepositoryParams)
	if cp.From.Repository != moveFrom || cp.From.Password != "old-password" || cp.To.Repository != moveTo || cp.To.Password != "new-password" {
		t.Fatalf("CopyRepository params = %+v", cp)
	}
	info, _ := agent.lastParams[agentrpc.RepositoryInfo.Name()].(rpc.RepositoryInfoParams)
	if info.Repository != moveTo || !slices.Equal(info.Expect, []string{"11111111aaaaaaaa", "22222222bbbbbbbb", "33333333cccccccc"}) {
		t.Fatalf("the check of the new repository = %+v", info)
	}
	repo, password, err := s.backupRepository()
	if err != nil || repo != moveTo || password != "new-password" {
		t.Fatalf("settings after the move = %q %q %v", repo, password, err)
	}
	if got := repositoryOf(t, s, records[0].ID, records[1].ID, records[2].ID); !slices.Equal(got, []string{moveTo, moveTo, moveTo}) {
		t.Fatalf("records after the move: %v", got)
	}
	// The newest backup of each site is tested from the new repository.
	waitForTasks(t, s)
	var verified []string
	agent.mu.Lock()
	for _, call := range agent.history {
		if p, ok := call.params.(rpc.RestoreParams); ok && call.method == agentrpc.VerifyBackup.Name() {
			if p.Repository != moveTo {
				t.Errorf("a restore test ran against %s", p.Repository)
			}
			verified = append(verified, p.SnapshotID)
		}
	}
	agent.mu.Unlock()
	slices.Sort(verified)
	if !slices.Equal(verified, []string{"22222222bbbbbbbb", "33333333cccccccc"}) {
		t.Fatalf("restore tests after the move: %v, want each site's newest", verified)
	}
	events, _ := s.Store.ListAuditEvents(20)
	var moved bool
	for _, e := range events {
		moved = moved || e.Action == "backup.moved" && strings.Contains(e.Detail, "3 backups moved")
	}
	if !moved {
		t.Fatalf("no backup.moved audit event: %+v", events)
	}
}

// §5.9: a failure before the transaction leaves the setting and every
// record on the old repository: a different repository for "moved", a
// recorded snapshot missing, a copy that fails, a new repository that does
// not exist.
func TestMoveRepositoryFailureMovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		ids     map[string]string
		missing map[string][]string
		copyErr error
		want    string
	}{
		{"another repository", "moved", map[string]string{moveFrom: repoIDA, moveTo: repoIDB}, nil, nil, "this is another repository (ID bbbbbbbb), not a copy of the current one (ID aaaaaaaa)"},
		{"missing snapshot", "moved", map[string]string{moveFrom: repoIDA, moveTo: repoIDA}, map[string][]string{moveTo: {"22222222bbbbbbbb"}}, nil, "1 recorded backups are not in the new repository (22222222bbbbbbbb)"},
		{"missing after a copy", "copy", map[string]string{moveFrom: repoIDA, moveTo: repoIDB}, map[string][]string{moveTo: {"11111111aaaaaaaa", "33333333cccccccc"}}, nil, "2 recorded backups are not in the new repository"},
		{"copy fails", "copy", map[string]string{moveFrom: repoIDA, moveTo: repoIDB}, nil, agentrpc.Invalid("the backup repository password is wrong"), "the copy failed"},
		{"no such repository", "moved", map[string]string{moveFrom: repoIDA}, nil, nil, "the new repository does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, agent, c, records := moveSetup(t)
			moveAgent(agent, tc.ids, tc.missing)
			if tc.copyErr != nil {
				agent.mu.Lock()
				agent.errs[agentrpc.CopyRepository.Name()] = tc.copyErr
				agent.mu.Unlock()
			}
			resp, body := c.do("POST", "/api/backups/repository/move", map[string]any{"mode": tc.mode, "repository": moveTo, "password": "new-password"})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("move = %d %s", resp.StatusCode, body)
			}
			j := waitTaskEnd(t, s, taskOf(t, body).ID)
			if j.Status != state.JobFailed || !strings.Contains(j.Error, tc.want) {
				t.Fatalf("move job = %s %q, want failed with %q", j.Status, j.Error, tc.want)
			}
			repo, password, _ := s.backupRepository()
			if repo != moveFrom || password != "old-password" {
				t.Fatalf("the setting moved: %q %q", repo, password)
			}
			if got := repositoryOf(t, s, records[0].ID, records[1].ID, records[2].ID); !slices.Equal(got, []string{moveFrom, moveFrom, moveFrom}) {
				t.Fatalf("records after a failed move: %v", got)
			}
			if tc.mode == "moved" && agent.called(agentrpc.CopyRepository.Name()) {
				t.Fatal("a move of files already copied copied them")
			}
		})
	}
}

// §5.9: a verify queued under the old repository's lock before the move
// runs under the new one's, against the repository the record names now.
func TestVerifyFollowsAMovedRecord(t *testing.T) {
	s, agent, c, records := moveSetup(t)
	agent.mu.Lock()
	agent.results[agentrpc.VerifyBackup.Name()] = rpc.VerifyResult{Passed: true}
	agent.mu.Unlock()
	release := holdLocks(t, s, jobs.RepoLock(moveFrom))
	resp, body := c.do("POST", "/api/backups/"+strconv.FormatInt(records[0].ID, 10)+"/verify", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("verify = %d %s", resp.StatusCode, body)
	}
	if _, err := s.Store.MoveBackupRepository(moveFrom, func(p *state.PanelSettings) error {
		p.BackupRepository = moveTo
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Still held: the verify does not wait for the old repository any more.
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	release()
	if j.Status != state.JobSucceeded {
		t.Fatalf("verify = %s %q", j.Status, j.Error)
	}
	if p, _ := agent.lastParams[agentrpc.VerifyBackup.Name()].(rpc.RestoreParams); p.Repository != moveTo {
		t.Fatalf("verify ran against %q", p.Repository)
	}
}
