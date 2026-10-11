package host

import (
	"bytes"
	"strings"
	"testing"
)

const siteDefiner = "`site_3`@`localhost`"

func rewrite(t *testing.T, in string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	n, err := RewriteDefiners(&out, strings.NewReader(in), siteDefiner)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), n
}

// SR-T7: every DEFINER clause of a dump names the site's own account, in
// every form mariadb-dump, mysqldump and people write.
func TestRewriteDefiners(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET NEW.i = 1 */;;",
			"/*!50003 CREATE*/ /*!50017 DEFINER=" + siteDefiner + "*/ /*!50003 TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET NEW.i = 1 */;;"},
		{"/*!50001 CREATE ALGORITHM=UNDEFINED */\n/*!50013 DEFINER=`evil`@`%` SQL SECURITY DEFINER */\n/*!50001 VIEW `v` AS select 1 */;",
			"/*!50001 CREATE ALGORITHM=UNDEFINED */\n/*!50013 DEFINER=" + siteDefiner + " SQL SECURITY DEFINER */\n/*!50001 VIEW `v` AS select 1 */;"},
		{"CREATE DEFINER='root'@'localhost' PROCEDURE p() SELECT 1;", "CREATE DEFINER=" + siteDefiner + " PROCEDURE p() SELECT 1;"},
		{"CREATE DEFINER = root@localhost FUNCTION f() RETURNS INT RETURN 1;", "CREATE DEFINER=" + siteDefiner + " FUNCTION f() RETURNS INT RETURN 1;"},
		{"CREATE definer=\"mysql.sys\"@\"localhost\" VIEW v AS SELECT 1;", "CREATE DEFINER=" + siteDefiner + " VIEW v AS SELECT 1;"},
		{"CREATE DEFINER=CURRENT_USER() TRIGGER t", "CREATE DEFINER=" + siteDefiner + " TRIGGER t"},
		{"CREATE DEFINER = current_user VIEW v", "CREATE DEFINER=" + siteDefiner + " VIEW v"},
		{"CREATE DEFINER=CURRENT_ROLE VIEW v", "CREATE DEFINER=" + siteDefiner + " VIEW v"},
		{"CREATE DEFINER=`some_role` VIEW v", "CREATE DEFINER=" + siteDefiner + " VIEW v"},
		{"CREATE DEFINER=`we``ird`@`h` VIEW v", "CREATE DEFINER=" + siteDefiner + " VIEW v"},
		{"/*M!100100 DEFINER=`root`@`localhost`*/", "/*M!100100 DEFINER=" + siteDefiner + "*/"},
	} {
		got, n := rewrite(t, tc.in)
		if got != tc.want || n != 1 {
			t.Errorf("%q\n got  %q (%d)\n want %q", tc.in, got, n, tc.want)
		}
	}
}

// What is not a DEFINER clause is copied byte for byte: strings (a post
// that quotes SQL), comments, SQL SECURITY DEFINER, a longer word.
func TestRewriteDefinersLeavesTheRest(t *testing.T) {
	for _, in := range []string{
		"INSERT INTO posts VALUES (1, 'DEFINER=`root`@`localhost`');",
		"INSERT INTO posts VALUES (1, \"DEFINER=root@localhost\");",
		"INSERT INTO posts VALUES (1, 'it\\'s DEFINER=root@x');",
		"INSERT INTO posts VALUES (1, 'it''s DEFINER=root@x');",
		"INSERT INTO posts VALUES (1, 'a\\\\'), (2, 'DEFINER=root@x');",
		"-- DEFINER=root@localhost\nSELECT 1;",
		"# DEFINER=root@localhost\nSELECT 1;",
		"/* DEFINER=root@localhost */ SELECT 1;",
		"/* it's */ SELECT 'DEFINER=root@x';",
		"CREATE VIEW v SQL SECURITY DEFINER AS SELECT 1;",
		"SELECT XDEFINER=1, `DEFINER=root@x` FROM t;",
		"SELECT 1 - -1; SELECT 'x';",
		"",
	} {
		got, n := rewrite(t, in)
		if got != in || n != 0 {
			t.Errorf("%q became %q (%d)", in, got, n)
		}
	}
}

// A clause across the reader's buffer boundary is still one clause; long
// dumps go through unchanged otherwise.
func TestRewriteDefinersAcrossBuffers(t *testing.T) {
	for _, at := range []int{64<<10 - 3, 64<<10 - 20, 64 << 10, 3*64<<10 + 7} {
		pad := strings.Repeat("x", at-len("INSERT INTO t VALUES ('')\n;")) // inside a string
		in := "INSERT INTO t VALUES ('" + pad + "')\n;CREATE DEFINER=`root`@`localhost` VIEW v AS SELECT 1;\n" + strings.Repeat("SELECT 1;\n", 10000)
		got, n := rewrite(t, in)
		want := strings.Replace(in, "DEFINER=`root`@`localhost`", "DEFINER="+siteDefiner, 1)
		if n != 1 || got != want {
			t.Errorf("at %d: %d replaced, equal %v", at, n, got == want)
		}
	}
}
