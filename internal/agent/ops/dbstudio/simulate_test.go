package dbstudio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/go-sql-driver/mysql"
)

// What may be simulated: one UPDATE, DELETE, INSERT or REPLACE. Anything
// that commits by itself, is not one statement or hides an executable
// comment is refused before a connection is opened.
func TestPlanSimulation(t *testing.T) {
	ok := map[string]string{
		"UPDATE wp_posts SET post_status = 'draft' WHERE ID > 3":           "update",
		"  update `wp_posts` set a = 1":                                    "update",
		"-- a comment\nDELETE FROM wp_posts WHERE ID = 1":                  "delete",
		"/* why */ DELETE LOW_PRIORITY FROM wp_posts":                      "delete",
		"INSERT INTO wp_log SELECT post_title FROM wp_posts":               "insert",
		"INSERT INTO wp_log (line) VALUES ('a;b'), ('c')":                  "insert",
		"REPLACE INTO wp_options (option_id, option_name) VALUES (1, 'x')": "replace",
		"# hash comment\nUPDATE wp_posts SET post_title = 'it''s; fine'":   "update",
	}
	for q, kind := range ok {
		sim, why := planSimulation(q)
		if why != "" || sim.kind != kind {
			t.Errorf("%q: %q %q", q, sim.kind, why)
		}
	}
	refused := map[string]string{
		"SELECT * FROM wp_posts":                          "changes nothing",
		"WITH x AS (SELECT 1) SELECT * FROM x":            "changes nothing",
		"CREATE TABLE x (a INT)":                          "commits by itself",
		"ALTER TABLE wp_posts ADD c INT":                  "commits by itself",
		"DROP TABLE wp_posts":                             "commits by itself",
		"TRUNCATE wp_posts":                               "commits by itself",
		"RENAME TABLE a TO b":                             "commits by itself",
		"LOCK TABLES wp_posts WRITE":                      "commits by itself",
		"SET autocommit = 1":                              "commits by itself",
		"GRANT ALL ON *.* TO x":                           "commits by itself",
		"COMMIT":                                          "commits by itself",
		"START TRANSACTION":                               "commits by itself",
		"CALL wc_restock()":                               "commits by itself",
		"LOAD DATA INFILE 'x' INTO TABLE t":               "commits by itself",
		"UPDATE wp_posts SET a = 1; COMMIT":               "one statement",
		"DELETE FROM wp_posts WHERE 1;DROP TABLE x":       "one statement",
		"/*!50000 DROP TABLE wp_posts */":                 "executable comment",
		"UPDATE wp_posts SET a = 1 /*! , b = 2 */":        "executable comment",
		"UPDATE wp_posts SET a = 1 /*M!100000 , b = 2 */": "executable comment",
		"UPDATE wp_posts SET a = 'open":                   "not closed",
		"UPDATE wp_posts SET a = (1":                      "parentheses",
		"UPDATE wp_posts SET a = 1 /* open":               "not closed",
		"HANDLER wp_posts OPEN":                           "commits by itself",
		"(SELECT 1)":                                      "only UPDATE",
		"FOO bar":                                         "only UPDATE",
	}
	for q, want := range refused {
		if _, why := planSimulation(q); !strings.Contains(why, want) {
			t.Errorf("%q: %q, want %q", q, why, want)
		}
	}
	// A string read differently with NO_BACKSLASH_ESCAPES: the names of
	// both readings are checked, and a ; hidden by one is seen by the other.
	if _, why := planSimulation(`UPDATE wp_posts SET a = 'x\'; DROP TABLE y; -- '`); why == "" {
		t.Error("a ; outside the string without backslash escapes was not seen")
	}
	sim, _ := planSimulation(`UPDATE wp_posts SET a = 'x\', b = (SELECT 1 FROM wp_options) -- '`)
	if !strings.Contains(strings.Join(sim.names, " "), "wp_options") {
		t.Errorf("names %v miss wp_options", sim.names)
	}
	// ANSI_QUOTES with backslash escapes: a backslash escapes in '…' but
	// not in "…" (a name), which neither reading above sees; a function
	// hidden from both is seen by the third.
	sim, _ = planSimulation(`UPDATE t SET v = 'q\'' WHERE "id\" = 0 OR EXISTS (SELECT 1 FROM hidden_tbl) -- " -- '`)
	if !strings.Contains(strings.Join(sim.names, " "), "hidden_tbl") {
		t.Errorf("names %v miss hidden_tbl", sim.names)
	}
	if _, why := planSimulation(`UPDATE t SET v = 'q\'' WHERE "id\" = 0; DROP TABLE x -- " -- '`); !strings.Contains(why, "one statement") {
		t.Errorf("a ; seen only under ANSI_QUOTES: %q", why)
	}
}

// Calls are an allowlist: built-ins without side effects, in every
// reading; anything else (a stored, package or loadable function, a
// built-in that reaches outside the transaction, a qualified name) is
// refused before a connection.
func TestSimulationCalls(t *testing.T) {
	ok := []string{
		"INSERT INTO wp_log (line) VALUES (CONCAT('a', NOW()))",
		"INSERT INTO site_4.wp_log (line) VALUES ('x')",
		"INSERT INTO `wp_log` (line) SELECT UPPER(post_title) FROM wp_posts",
		"INSERT wp_log () VALUES ()",
		"REPLACE INTO wp_options (option_id, option_name) VALUES (1, 'x')",
		"INSERT INTO wp_options (option_id, option_name) VALUES (1, 'x') ON DUPLICATE KEY UPDATE option_name = VALUES(option_name)",
		"UPDATE wp_posts SET post_title = CAST(ID AS DECIMAL(10, 2)) WHERE ID IN (SELECT MAX(option_id) FROM wp_options)",
		"UPDATE wp_posts p JOIN wp_options o USING (ID) SET p.a = COALESCE(o.b, IFNULL(NULL, 1)) WHERE NOT (p.c AND (p.d OR p.e))",
		"DELETE FROM wp_posts WHERE EXISTS (SELECT 1 FROM wp_options WHERE JSON_VALUE(option_value, '$.a') = 1)",
		"UPDATE wp_posts SET a = IF(DATE_ADD(NOW(), INTERVAL 1 DAY) > '2026-01-01', 1, 0)",
		"UPDATE wp_posts SET a = (SELECT COUNT(*) FROM wp_options)",
	}
	for _, q := range ok {
		if _, why := planSimulation(q); why != "" {
			t.Errorf("%q refused: %s", q, why)
		}
	}
	refused := []string{
		"UPDATE wp_posts SET post_title = f_slug(post_title)",
		"UPDATE wp_posts SET post_title = pk.f(post_title)",
		"UPDATE wp_posts SET post_title = site_4.concat('a')",
		"UPDATE wp_posts SET post_title = `concat`('a')",
		"UPDATE wp_posts SET post_title = sys_exec('x')",
		"UPDATE wp_posts SET a = SLEEP(5)",
		"UPDATE wp_posts SET a = BENCHMARK(1000, MD5('x'))",
		"UPDATE wp_posts SET a = GET_LOCK('x', 1)",
		"UPDATE wp_posts SET a = RELEASE_LOCK('x')",
		"UPDATE wp_posts SET a = IS_USED_LOCK('x')",
		"UPDATE wp_posts SET a = LOAD_FILE('/etc/passwd')",
		"INSERT INTO wp_log VALUES (NEXTVAL(s))",
		"INSERT INTO wp_log VALUES (SETVAL(s, 9))",
		"UPDATE wp_posts SET a = LAST_INSERT_ID(7)",
		"UPDATE wp_posts SET a = MASTER_POS_WAIT('f', 4)",
		"UPDATE wp_posts SET a = UUID_SHORT()",
		"UPDATE wp_posts SET a = sleep (1)",
	}
	for _, q := range refused {
		if _, why := planSimulation(q); !strings.Contains(why, "built-in functions only") {
			t.Errorf("%q: %q", q, why)
		}
	}
	// A call only one reading sees (ANSI_QUOTES) is refused too.
	if _, why := planSimulation(`UPDATE t SET v = 'q\'' WHERE "id\" = 0 OR logit() -- " -- '`); !strings.Contains(why, "logit") {
		t.Errorf("a call hidden from two readings: %q", why)
	}
}

// Shapes are an allowlist too: UPDATE and DELETE (one table or several),
// INSERT and REPLACE with VALUES, SELECT or SET, RETURNING or not.
func TestSimulationShapes(t *testing.T) {
	ok := []string{
		"UPDATE wp_posts, wp_options SET wp_posts.a = wp_options.b WHERE wp_posts.ID = wp_options.option_id",
		"DELETE wp_posts FROM wp_posts JOIN wp_options ON wp_options.option_id = wp_posts.ID",
		"DELETE FROM wp_posts WHERE ID = 1 RETURNING ID",
		"INSERT INTO wp_log VALUES ('x') RETURNING line",
		"INSERT INTO wp_log PARTITION (p0) (line) VALUES ('x')",
		"INSERT INTO wp_log (SELECT post_title FROM wp_posts)",
		"INSERT INTO wp_log SET line = 'x'",
		"REPLACE wp_log (line) VALUE ('x')",
		"UPDATE wp_posts SET a = SUBSTRING(b FROM 1 FOR 3)",
	}
	for _, q := range ok {
		if _, why := planSimulation(q); why != "" {
			t.Errorf("%q refused: %s", q, why)
		}
	}
	refused := map[string]string{
		"INSERT INTO wp_log TABLE wp_posts":                                        "VALUES, SELECT or SET only",
		"INSERT INTO wp_log WITH x AS (SELECT 1) SELECT * FROM x":                  "VALUES, SELECT or SET only",
		"INSERT INTO wp_log SELECT post_title FROM wp_posts INTO @x":               "INTO a variable",
		"INSERT INTO wp_log SELECT post_title FROM wp_posts INTO OUTFILE '/tmp/x'": "INTO a variable",
		"UPDATE wp_posts SET a = (SELECT b FROM t PROCEDURE ANALYSE())":            "PROCEDURE",
		"UPDATE wp_posts SET a = @x := a + 1":                                      "user variable",
		"UPDATE wp_posts SET a = (SELECT b FROM wp_options FOR UPDATE)":            "locking read",
		"DELETE FROM wp_posts WHERE ID IN (SELECT ID FROM t FOR SHARE)":            "locking read",
		"UPDATE wp_posts SET a = (SELECT b FROM t LOCK IN SHARE MODE)":             "locking read",
		"DELETE HISTORY FROM wp_posts":                                             "HISTORY",
		"SET STATEMENT sql_mode = 'ANSI_QUOTES' FOR UPDATE wp_posts SET a = 1":     "SET STATEMENT",
	}
	for q, want := range refused {
		if _, why := planSimulation(q); !strings.Contains(why, want) {
			t.Errorf("%q: %q, want %q", q, why, want)
		}
	}
}

// Every name a table may have is checked, as written (the server lowers
// it): all digits when quoted or after a name's dot, up to 64 characters
// however many bytes; a number is not a name.
func TestSimulationNames(t *testing.T) {
	cyr := strings.Repeat("б", 64)
	sim, why := planSimulation("UPDATE t, `123`, db.456, db . 789, `" + cyr + "`, `İx` SET t.a = 1.5, t.b = 42 WHERE `" + cyr + "б` = 0")
	if why != "" {
		t.Fatal(why)
	}
	got := map[string]bool{}
	for _, n := range sim.names {
		got[n] = true
	}
	for _, want := range []string{"t", "123", "db", "456", "789", cyr, "İx", "UPDATE"} {
		if !got[want] {
			t.Errorf("names %q miss %q", sim.names, want)
		}
	}
	for _, not := range []string{"1", "5", "42", cyr + "б"} {
		if got[not] {
			t.Errorf("names %q hold %q", sim.names, not)
		}
	}
}

// The preview is a SELECT of the statement's own tables and WHERE, for
// one-target UPDATE and DELETE only.
func TestPreviewSelect(t *testing.T) {
	cases := map[string]string{
		"UPDATE wp_posts SET post_status = 'draft' WHERE ID > 3 ORDER BY ID LIMIT 5":      "SELECT * FROM wp_posts WHERE ID > 3 ORDER BY ID LIMIT 5",
		"UPDATE LOW_PRIORITY IGNORE `wp_posts` SET a = 1":                                 "SELECT * FROM `wp_posts`",
		"UPDATE wp_posts p JOIN wp_options o ON o.option_id = p.ID SET p.a = 1 WHERE o.x": "SELECT * FROM wp_posts p JOIN wp_options o ON o.option_id = p.ID WHERE o.x",
		"UPDATE wp_posts SET a = (SELECT b FROM t WHERE c) WHERE d = 'WHERE'":             "SELECT * FROM wp_posts WHERE d = 'WHERE'",
		"DELETE FROM wp_posts WHERE ID IN (1, 2)":                                         "SELECT * FROM wp_posts WHERE ID IN (1, 2)",
		"DELETE QUICK FROM wp_posts LIMIT 3":                                              "SELECT * FROM wp_posts LIMIT 3",
		"DELETE FROM wp_posts WHERE ID = 1 RETURNING ID":                                  "SELECT * FROM wp_posts WHERE ID = 1",
		"DELETE FROM wp_posts":                                                            "SELECT * FROM wp_posts",
		"DELETE wp_posts FROM wp_posts JOIN t ON t.a = wp_posts.ID":                       "",
		"DELETE FROM wp_posts USING wp_posts JOIN t":                                      "",
		"INSERT INTO wp_log SELECT * FROM wp_posts":                                       "",
		`UPDATE wp_posts SET a = 'x\' WHERE b = 1 -- '`:                                   "",
	}
	for q, want := range cases {
		sim, why := planSimulation(q)
		if why != "" {
			t.Errorf("%q refused: %s", q, why)
			continue
		}
		if sim.preview != want {
			t.Errorf("%q:\n got %q\nwant %q", q, sim.preview, want)
		}
	}
}

// statements is the first word of every statement the fake received, in
// the order sent (queries and execs interleaved by their kind is enough
// here: execs are the transaction's own).
func (fx *fixture) execWords() []string {
	fx.f.mu.Lock()
	defer fx.f.mu.Unlock()
	var out []string
	for _, e := range fx.f.execs {
		out = append(out, strings.Fields(e.q)[0])
	}
	return out
}

func (fx *fixture) rootZero() {
	scan := map[string][]string{}
	for k, v := range fx.m.Rows {
		scan[k] = v
	}
	fx.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if strings.Contains(q, "information_schema.TRIGGERS") || strings.Contains(q, "information_schema.ROUTINES") || strings.Contains(q, "mysql.func") {
			return []string{"0"}, nil
		}
		return scan[q], nil
	}
}

// A simulation runs START TRANSACTION, the preview, the statement and
// ROLLBACK, on every path; it never sends COMMIT, and it closes its
// connection.
func TestSimulateAlwaysRollsBack(t *testing.T) {
	update := "UPDATE wp_posts SET post_status = 'draft' WHERE ID < 20"
	t.Run("success", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				return 4, nil
			}
			return 0, nil
		}
		fx.f.selectRows = [][]*string{{sp("12")}, {sp("13")}}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update + ";"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Refused != "" || res.Affected != 4 || !res.RolledBack || res.Kind != "update" || res.Preview == nil || len(res.Preview.Rows) != 2 ||
			res.PreviewSQL != "SELECT * FROM wp_posts WHERE ID < 20" {
			t.Fatalf("result %+v", res)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START SET UPDATE ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
		if fx.f.closed != 1 {
			t.Fatalf("closed %d connections", fx.f.closed)
		}
		// The write needs a write account; the caller may write here.
		if fx.f.readOnly[len(fx.f.readOnly)-1] {
			t.Fatal("a caller who may write got a read-only account")
		}
	})
	t.Run("a long preview", func(t *testing.T) {
		// More rows than shown are read to the end, never cut off (that
		// drops the connection the transaction is on), and the server is
		// asked for one more than shown.
		fx := newFixture(t)
		fx.rootZero()
		for i := range 30 {
			fx.f.selectRows = append(fx.f.selectRows, []*string{sp(fmt.Sprint(i))})
		}
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				return 30, nil
			}
			return 0, nil
		}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		if err != nil {
			t.Fatal(err)
		}
		if res.Affected != 30 || res.Preview == nil || len(res.Preview.Rows) != previewRows || !res.Preview.Truncated || !res.RolledBack {
			t.Fatalf("result %+v", res)
		}
		var sent string
		for _, q := range fx.f.queries {
			if strings.Contains(q.q, "FROM wp_posts WHERE") {
				sent = q.q
			}
		}
		var secs float64
		if _, err := fmt.Sscanf(sent, "SET STATEMENT sql_select_limit = 26, max_statement_time = %f FOR SELECT * FROM wp_posts WHERE ID < 20", &secs); err != nil || secs > 55 || secs < 50 {
			t.Fatalf("preview sent as %q", sent)
		}
	})
	t.Run("one budget", func(t *testing.T) {
		// The preview and the statement share one budget: the statement's
		// server-side limit is what the preview left of it, and with too
		// little left it does not run at all.
		fx := newFixture(t)
		fx.rootZero()
		var limits []float64
		fx.f.exec = func(q string, _ []any) (int64, error) {
			var secs float64
			if _, err := fmt.Sscanf(q, "SET SESSION max_statement_time = %f", &secs); err == nil {
				limits = append(limits, secs)
			}
			return 1, nil
		}
		if _, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update}); err != nil {
			t.Fatal(err)
		}
		if len(limits) != 1 || limits[0] > 55 || limits[0] < 50 {
			t.Fatalf("the statement's limits: %v", limits)
		}
		defer func(b time.Duration) { simBudget = b }(simBudget)
		simBudget = simMargin + 500*time.Millisecond
		fx = newFixture(t)
		fx.rootZero()
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Message, "used up") || !res.RolledBack {
			t.Fatalf("result %+v", res)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
	})
	t.Run("returning", func(t *testing.T) {
		// RETURNING answers a row per row changed, and no count: the
		// rows are counted.
		fx := newFixture(t)
		fx.rootZero()
		fx.f.selectRows = [][]*string{{sp("1")}, {sp("2")}, {sp("3")}}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: "DELETE FROM wp_posts WHERE ID < 4 RETURNING ID"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Affected != 3 || !res.RolledBack {
			t.Fatalf("result %+v", res)
		}
		if len(fx.f.execs) != 4 || fx.f.execs[3].q != "ROLLBACK" {
			t.Fatalf("execs %v", fx.f.execs)
		}
	})
	t.Run("read-only caller", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				return 0, &mysql.MySQLError{Number: 1142, Message: "UPDATE command denied"}
			}
			return 0, nil
		}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if !fx.f.readOnly[len(fx.f.readOnly)-1] || !strings.Contains(res.Message, "1142") || !res.RolledBack {
			t.Fatalf("result %+v, read-only %v", res, fx.f.readOnly)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START SET UPDATE ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
	})
	t.Run("start fails", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if q == "START TRANSACTION" {
				return 0, &mysql.MySQLError{Number: 1792, Message: "cannot start"}
			}
			return 1, nil
		}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		if err != nil {
			t.Fatal(err)
		}
		if res.RolledBack || res.Message == "" {
			t.Fatalf("result %+v", res)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START" {
			t.Fatalf("the statement was sent without a transaction: %s", got)
		}
	})
	t.Run("no transaction open", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.inTransaction = "0"
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		if err != nil {
			t.Fatal(err)
		}
		if res.Refused == "" {
			t.Fatalf("result %+v", res)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
	})
	t.Run("statement fails", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				return 0, &mysql.MySQLError{Number: 1054, Message: "Unknown column 'nope'"}
			}
			return 0, nil
		}
		res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Message, "Unknown column") || !res.RolledBack {
			t.Fatalf("result %+v", res)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START SET UPDATE ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
	})
	t.Run("request cancelled", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		cctx, cancel := context.WithCancel(ctx)
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				cancel()
				return 0, context.Canceled
			}
			return 0, nil
		}
		if _, err := fx.o.Simulate(cctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if got := strings.Join(fx.execWords(), " "); got != "SET START SET UPDATE ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
		if fx.f.closed != 1 {
			t.Fatalf("closed %d connections", fx.f.closed)
		}
	})
	t.Run("rollback fails", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if q == "ROLLBACK" {
				return 0, errors.New("connection lost")
			}
			return 2, nil
		}
		if _, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update}); err == nil || !strings.Contains(err.Error(), "connection was closed") {
			t.Fatalf("err = %v", err)
		}
		if fx.f.closed != 1 {
			t.Fatalf("closed %d connections", fx.f.closed)
		}
	})
	t.Run("panic", func(t *testing.T) {
		fx := newFixture(t)
		fx.rootZero()
		fx.f.exec = func(q string, _ []any) (int64, error) {
			if strings.HasPrefix(q, "UPDATE") {
				panic("boom")
			}
			return 0, nil
		}
		func() {
			defer func() { _ = recover() }()
			_, _ = fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: update})
		}()
		if got := strings.Join(fx.execWords(), " "); got != "SET START SET UPDATE ROLLBACK" {
			t.Fatalf("statements: %s", got)
		}
		if fx.f.closed != 1 {
			t.Fatalf("closed %d connections", fx.f.closed)
		}
	})
}

// What a rollback cannot undo is refused before the transaction: a
// table that is not InnoDB, a view, triggers, stored functions, a
// statement that names no table this user sees.
func TestSimulateRefusesWhatCannotRollBack(t *testing.T) {
	cases := map[string]struct {
		sql    string
		setup  func(fx *fixture)
		refuse string
	}{
		"myisam": {sql: "DELETE FROM wp_log", setup: func(fx *fixture) { fx.f.tables["wp_log"].engine = "MyISAM" }, refuse: "MyISAM engine"},
		"myisam read in a subquery": {sql: "UPDATE wp_posts SET post_title = (SELECT line FROM wp_log LIMIT 1)",
			setup: func(fx *fixture) { fx.f.tables["wp_log"].engine = "Aria" }, refuse: "Aria engine"},
		"view":        {sql: "UPDATE v_posts SET ID = 1", refuse: "is a view"},
		"no table":    {sql: "UPDATE nothing_here SET a = 1", refuse: "names no table"},
		"memory":      {sql: "INSERT INTO wp_log VALUES ('x')", setup: func(fx *fixture) { fx.f.tables["wp_log"].engine = "MEMORY" }, refuse: "MEMORY engine"},
		"triggers":    {sql: "DELETE FROM wp_posts", setup: func(fx *fixture) { fx.rootAnswer("TRIGGERS", "1") }, refuse: "triggers"},
		"function":    {sql: "UPDATE wp_posts SET post_title = value(post_title)", setup: func(fx *fixture) { fx.rootAnswer("ROUTINES", "1") }, refuse: "stored function"},
		"package":     {sql: "UPDATE wp_posts SET post_title = pk", setup: func(fx *fixture) { fx.rootAnswer("ROUTINES", "1") }, refuse: "package"},
		"udf":         {sql: "UPDATE wp_posts SET post_title = sys_exec", setup: func(fx *fixture) { fx.rootAnswerFrom("mysql.func", "1") }, refuse: "loadable function"},
		"one at once": {sql: "UPDATE wp_posts SET a = 1; COMMIT", refuse: "one statement"},
		"ansi quotes": {sql: "UPDATE wp_posts SET a = 1", setup: func(fx *fixture) {
			fx.f.sqlMode = "REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ANSI"
		}, refuse: "ANSI_QUOTES"},
		"no backslash escapes": {sql: "UPDATE wp_posts SET a = 1", setup: func(fx *fixture) {
			fx.f.sqlMode = "STRICT_TRANS_TABLES,NO_BACKSLASH_ESCAPES"
		}, refuse: "NO_BACKSLASH_ESCAPES"},
		"ddl": {sql: "DROP TABLE wp_posts", refuse: "cannot be simulated"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t)
			fx.rootZero()
			if c.setup != nil {
				c.setup(fx)
			}
			res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: c.sql})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Refused, c.refuse) {
				t.Fatalf("refused %q, want %q", res.Refused, c.refuse)
			}
			if len(fx.f.execs) != 0 {
				t.Fatalf("a refused simulation sent %v", fx.f.execs)
			}
		})
	}
}

// Names that match more tables than the check reads (case variants on a
// case-sensitive server) refuse the statement: the rows past the limit
// would go unchecked.
func TestSimulateRefusesTooManyTables(t *testing.T) {
	fx := newFixture(t)
	fx.rootZero()
	var names []string
	for i := range 700 {
		n := fmt.Sprintf("abcd%03d", i)
		names = append(names, n)
		for v := range 16 {
			b := []byte(n)
			for j := range 4 {
				if v&(1<<j) != 0 {
					b[j] -= 'a' - 'A'
				}
			}
			fx.f.tables[string(b)] = &fakeTable{typ: "BASE TABLE"}
		}
	}
	res, err := fx.o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: fx.site, SQL: "UPDATE " + strings.Join(names, ", ") + " SET x = 1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Refused, "more than 10000 tables") || len(fx.f.execs) != 0 {
		t.Fatalf("result %+v, execs %v", res, fx.f.execs)
	}
}

// rootAnswer makes the root count of one information_schema table n.
func (fx *fixture) rootAnswer(table, n string) { fx.rootAnswerFrom("information_schema."+table, n) }

// rootAnswerFrom makes the root count of a query from table n.
func (fx *fixture) rootAnswerFrom(table, n string) {
	prev := fx.m.OnQuery
	fx.m.OnQuery = func(q string, args []any) ([]string, error) {
		if strings.Contains(q, "FROM "+table+" ") {
			return []string{n}, nil
		}
		return prev(q, args)
	}
}
