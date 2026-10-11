package dbstudio

import (
	"fmt"
	"strings"
)

// What a simulation may contain is an allowlist (SPEC §12): the next trick
// nobody thought of is refused by default. The lookups of rollbackable
// (tables, triggers, routines, UDFs) stay as a second line.

// builtinFuncs are the server's own functions a simulated statement may
// call: pure ones, or ones whose only effect is their answer (RAND, NOW,
// UUID). A built-in name always resolves to the built-in, never to a
// stored function of the same name; a qualified name (db.f) never is one.
//
// Left out on purpose: SLEEP and BENCHMARK (they hold the transaction's
// locks while they run), GET_LOCK, RELEASE_LOCK, RELEASE_ALL_LOCKS,
// IS_FREE_LOCK, IS_USED_LOCK (locks outside the transaction), LOAD_FILE,
// NEXTVAL, LASTVAL, SETVAL (sequences do not roll back), LAST_INSERT_ID
// (with an argument it sets the session's value), MASTER_POS_WAIT,
// MASTER_GTID_WAIT, BINLOG_GTID_POS, UUID_SHORT (it advances a counter of
// the server's, outside the transaction), and every stored function,
// package function and UDF.
var builtinFuncs = setOf(
	// strings
	"ASCII", "BIN", "BIT_LENGTH", "CHAR", "CHAR_LENGTH", "CHARACTER_LENGTH", "CHR", "CONCAT", "CONCAT_WS",
	"ELT", "EXPORT_SET", "EXTRACTVALUE", "FIELD", "FIND_IN_SET", "FORMAT", "FROM_BASE64", "HEX", "INSERT",
	"INSTR", "LCASE", "LEFT", "LENGTH", "LENGTHB", "LOCATE", "LOWER", "LPAD", "LTRIM", "MAKE_SET", "MID",
	"NATURAL_SORT_KEY", "OCT", "OCTET_LENGTH", "ORD", "POSITION", "QUOTE", "REPEAT", "REPLACE", "REVERSE",
	"RIGHT", "RPAD", "RTRIM", "SFORMAT", "SOUNDEX", "SPACE", "STRCMP", "SUBSTR", "SUBSTRING",
	"SUBSTRING_INDEX", "TO_BASE64", "TO_CHAR", "TRIM", "UCASE", "UNHEX", "UPDATEXML", "UPPER",
	"WEIGHT_STRING", "REGEXP_INSTR", "REGEXP_REPLACE", "REGEXP_SUBSTR", "CHARSET", "COLLATION",
	"COERCIBILITY", "INET_ATON", "INET_NTOA", "INET6_ATON", "INET6_NTOA", "IS_IPV4", "IS_IPV6",
	"IS_IPV4_COMPAT", "IS_IPV4_MAPPED", "CRC32", "CRC32C", "MD5", "SHA", "SHA1", "SHA2", "COMPRESS",
	"UNCOMPRESS", "UNCOMPRESSED_LENGTH", "AES_ENCRYPT", "AES_DECRYPT", "RANDOM_BYTES",
	// numbers
	"ABS", "ACOS", "ASIN", "ATAN", "ATAN2", "CEIL", "CEILING", "CONV", "COS", "COT", "DEGREES", "EXP",
	"FLOOR", "GREATEST", "LEAST", "LN", "LOG", "LOG10", "LOG2", "MOD", "PI", "POW", "POWER", "RADIANS",
	"RAND", "ROUND", "SIGN", "SIN", "SQRT", "TAN", "TRUNCATE", "BIT_COUNT",
	// dates and times
	"ADDDATE", "ADDTIME", "ADD_MONTHS", "CONVERT_TZ", "CURDATE", "CURRENT_DATE", "CURRENT_TIME",
	"CURRENT_TIMESTAMP", "CURTIME", "DATE", "DATEDIFF", "DATE_ADD", "DATE_FORMAT", "DATE_SUB", "DAY",
	"DAYNAME", "DAYOFMONTH", "DAYOFWEEK", "DAYOFYEAR", "EXTRACT", "FROM_DAYS", "FROM_UNIXTIME",
	"GET_FORMAT", "HOUR", "LAST_DAY", "LOCALTIME", "LOCALTIMESTAMP", "MAKEDATE", "MAKETIME",
	"MICROSECOND", "MINUTE", "MONTH", "MONTHNAME", "NOW", "PERIOD_ADD", "PERIOD_DIFF", "QUARTER",
	"SECOND", "SEC_TO_TIME", "STR_TO_DATE", "SUBDATE", "SUBTIME", "SYSDATE", "TIME", "TIMEDIFF",
	"TIMESTAMP", "TIMESTAMPADD", "TIMESTAMPDIFF", "TIME_FORMAT", "TIME_TO_SEC", "TO_DAYS", "TO_SECONDS",
	"UNIX_TIMESTAMP", "UTC_DATE", "UTC_TIME", "UTC_TIMESTAMP", "WEEK", "WEEKDAY", "WEEKOFYEAR", "YEAR",
	"YEARWEEK",
	// control flow, comparison, casts and the types a CAST names
	"IF", "IFNULL", "NULLIF", "COALESCE", "NVL", "NVL2", "ISNULL", "INTERVAL", "CAST", "CONVERT",
	"DECIMAL", "DEC", "NUMERIC", "DOUBLE", "FLOAT", "DATETIME", "VARCHAR", "NCHAR", "BINARY", "VARBINARY",
	"DEFAULT", "ROW",
	// JSON
	"JSON_ARRAY", "JSON_ARRAYAGG", "JSON_ARRAY_APPEND", "JSON_ARRAY_INSERT", "JSON_ARRAY_INTERSECT",
	"JSON_COMPACT", "JSON_CONTAINS", "JSON_CONTAINS_PATH", "JSON_DEPTH", "JSON_DETAILED", "JSON_EQUALS",
	"JSON_EXISTS", "JSON_EXTRACT", "JSON_INSERT", "JSON_KEYS", "JSON_KEY_VALUE", "JSON_LENGTH",
	"JSON_LOOSE", "JSON_MERGE", "JSON_MERGE_PATCH", "JSON_MERGE_PRESERVE", "JSON_NORMALIZE",
	"JSON_OBJECT", "JSON_OBJECTAGG", "JSON_OBJECT_FILTER_KEYS", "JSON_OBJECT_TO_ARRAY", "JSON_OVERLAPS",
	"JSON_PRETTY", "JSON_QUERY", "JSON_QUOTE", "JSON_REMOVE", "JSON_REPLACE", "JSON_SCHEMA_VALID",
	"JSON_SEARCH", "JSON_SET", "JSON_TABLE", "JSON_TYPE", "JSON_UNQUOTE", "JSON_VALID", "JSON_VALUE",
	// dynamic columns
	"COLUMN_ADD", "COLUMN_CHECK", "COLUMN_CREATE", "COLUMN_DELETE", "COLUMN_EXISTS", "COLUMN_GET",
	"COLUMN_JSON", "COLUMN_LIST",
	// aggregates and window functions
	"COUNT", "SUM", "AVG", "MIN", "MAX", "GROUP_CONCAT", "BIT_AND", "BIT_OR", "BIT_XOR", "STD", "STDDEV",
	"STDDEV_POP", "STDDEV_SAMP", "VARIANCE", "VAR_POP", "VAR_SAMP", "ROW_NUMBER", "RANK", "DENSE_RANK",
	"NTILE", "LAG", "LEAD", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE", "CUME_DIST", "PERCENT_RANK",
	"PERCENTILE_CONT", "PERCENTILE_DISC", "MEDIAN",
	// what the session is
	"DATABASE", "SCHEMA", "USER", "CURRENT_USER", "SESSION_USER", "SYSTEM_USER", "CURRENT_ROLE",
	"CONNECTION_ID", "VERSION", "FOUND_ROWS", "ROW_COUNT", "UUID", "SYS_GUID",
)

// parenWords are the words a ( may follow that are not a call: operators,
// clauses, index hints, a CTE's or JSON_TABLE's parts.
var parenWords = setOf(
	"IN", "VALUES", "VALUE", "EXISTS", "ON", "AND", "OR", "NOT", "XOR", "WHERE", "USING", "PARTITION",
	"INDEX", "KEY", "SELECT", "FROM", "JOIN", "STRAIGHT_JOIN", "BY", "DISTINCT", "ALL", "ANY", "SOME", "LIKE",
	"BETWEEN", "THEN", "ELSE", "WHEN", "CASE", "RETURNING", "DIV", "REGEXP", "RLIKE", "ESCAPE", "HAVING",
	"UNION", "EXCEPT", "INTERSECT", "AS", "IS", "OVER", "AGAINST", "MATCH", "GROUP", "COLUMNS", "UPDATE",
	"IGNORE", "LOW_PRIORITY", "SET",
)

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// insertTarget is the index of the table an INSERT or REPLACE writes
// (its column list follows it in parentheses, not a call), and of its INTO
// (-1 without one); -1, -1 for any other statement or when there is none.
func insertTarget(list []token) (target, into int) {
	target, into = -1, -1
	if len(list) == 0 || !isWord(list[0], "INSERT", "REPLACE") {
		return
	}
	i := 1
	for i < len(list) && isWord(list[i], "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY", "IGNORE") {
		i++
	}
	if i < len(list) && isWord(list[i], "INTO") {
		into = i
		i++
	}
	if i >= len(list) || !isName(list[i]) {
		return -1, into
	}
	target = i
	if i+2 < len(list) && list[i+1].kind == tPunct && list[i+1].text == "." && isName(list[i+2]) {
		target = i + 2
	}
	return target, into
}

func isWord(t token, words ...string) bool {
	if t.kind != tWord {
		return false
	}
	for _, w := range words {
		if strings.EqualFold(t.text, w) {
			return true
		}
	}
	return false
}

// isName reports whether t may be a name: a word, a `name`, or a "name"
// (under ANSI_QUOTES).
func isName(t token) bool { return t.kind == tWord || t.kind == tName || t.kind == tString }

// checkCalls refuses a statement that calls anything but a built-in of
// builtinFuncs, in one lexing: every name a ( follows that is not a
// clause's word or the INSERT's table.
func checkCalls(stmt string, list []token) string {
	target, _ := insertTarget(list)
	for k := 1; k < len(list); k++ {
		if list[k].kind != tPunct || list[k].text != "(" {
			continue
		}
		p := list[k-1]
		if k-1 == target || p.kind == tPunct || (p.kind == tString && stmt[p.start] != '"') {
			continue
		}
		dotted := k >= 2 && list[k-2].kind == tPunct && list[k-2].text == "."
		name := strings.ToUpper(p.text)
		if p.kind == tWord && !dotted && (parenWords[name] || builtinFuncs[name]) {
			continue
		}
		return fmt.Sprintf("Simulate supports built-in functions only: %s(…) is not one it allows", p.text)
	}
	return ""
}

// checkShape refuses, in one lexing, a statement of a shape a simulation
// does not take: an INSERT or REPLACE other than INTO <table> [PARTITION
// (…)] [(columns)] VALUES / SELECT / SET; DELETE HISTORY; an INTO other
// than the INSERT's (a variable, a file); PROCEDURE; a user variable
// assigned (:=); a locking read (FOR UPDATE, FOR SHARE, LOCK IN SHARE
// MODE), which locks rows the statement does not change.
func checkShape(list []token, kind string) string {
	target, into := insertTarget(list)
	switch kind {
	case "insert", "replace":
		if target < 0 {
			return "simulate an INSERT or REPLACE INTO a table"
		}
		j := target + 1
		skip := func() { // past the parenthesis at j, to the token after its )
			d := list[j].depth
			for j++; j < len(list) && !(list[j].kind == tPunct && list[j].text == ")" && list[j].depth == d); j++ {
			}
			j++
		}
		if j < len(list) && isWord(list[j], "PARTITION") && j+1 < len(list) && list[j+1].text == "(" {
			j++
			skip()
		}
		if j < len(list) && list[j].kind == tPunct && list[j].text == "(" && !(j+1 < len(list) && isWord(list[j+1], "SELECT")) {
			skip()
		}
		switch {
		case j < len(list) && isWord(list[j], "VALUES", "VALUE", "SELECT", "SET"):
		case j+1 < len(list) && list[j].kind == tPunct && list[j].text == "(" && isWord(list[j+1], "SELECT"):
		default:
			return "simulate INSERT and REPLACE with VALUES, SELECT or SET only"
		}
	case "delete":
		for i := 1; i < len(list) && list[i].kind == tWord; i++ {
			if isWord(list[i], "HISTORY") {
				return "DELETE HISTORY cannot be simulated"
			}
			if !isWord(list[i], "LOW_PRIORITY", "QUICK", "IGNORE") {
				break
			}
		}
	}
	for i, t := range list {
		next := func(words ...string) bool { return i+1 < len(list) && isWord(list[i+1], words...) }
		switch {
		case isWord(t, "INTO") && i != into:
			return "INTO a variable or a file cannot be simulated"
		case isWord(t, "PROCEDURE"):
			return "PROCEDURE cannot be simulated"
		case t.kind == tPunct && t.text == ":" && i+1 < len(list) && list[i+1].kind == tPunct && list[i+1].text == "=":
			return "a statement that assigns a user variable (:=) cannot be simulated"
		case isWord(t, "FOR") && next("UPDATE", "SHARE"), isWord(t, "LOCK") && next("IN"):
			return "a locking read (FOR UPDATE, FOR SHARE, LOCK IN SHARE MODE) cannot be simulated: it locks rows the statement does not change"
		}
	}
	return ""
}
