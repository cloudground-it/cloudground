package cronjob

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestValidateSchedule(t *testing.T) {
	for _, ok := range []string{
		"* * * * *", "*/15 * * * *", "0 3 * * 0", "0 3 * * 7", "0-30/5 1,2,3 1-31 1-12 0-7",
		"59 23 31 12 7", "0\t3 * * *", "*/1 * * * *", "*/60 * * * *", "  5 4 * * 1-5 ",
	} {
		if err := ValidateSchedule(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 0 *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "5/10 * * * *", "30-10 * * * *",
		"* * * * *\n* * * * * id", "* * * * *\r", "@daily", "0 3 * * mon", "* * * jan *", "1,,2 * * * *",
		"- * * * *", "1- * * * *", "*/ * * * *", "0001 * * * *", "* * * * * ; id", "$(id) * * * *",
	} {
		if err := ValidateSchedule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateCommand(t *testing.T) {
	for _, ok := range []string{"echo hi", "php artisan schedule:run # nightly", "date +%Y\tx", "echo 'é'"} {
		if err := ValidateCommand(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "a\nb", "a\rb", "a\x00b", "a\x1bb", "a\x7fb", "a\u0085b", "\xff"} {
		if err := ValidateCommand(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLineHasNoPercentAndQuotes(t *testing.T) {
	l, err := Job{ID: 4, Schedule: "*/5  *  * * *", Command: `date +%Y-%m; echo 'it''s' \% 50%`}.Line()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l, "*/5 * * * * p=$(printf '\\045');/bin/sh -c ") {
		t.Fatalf("line %s", l)
	}
	if strings.Contains(l, "%") {
		t.Fatalf("a %% reached the crontab line: %s", l)
	}
	if !strings.HasSuffix(l, " logs/cron-4.log") {
		t.Fatalf("line %s", l)
	}
	l, _ = Job{ID: 5, Schedule: "* * * * *", Command: "true"}.Line()
	if strings.Contains(l, "printf") {
		t.Fatalf("the percent helper is added to every line: %s", l)
	}
}

// What cron hands to /bin/sh, run through a real shell, gives the job
// exactly the command the operator typed. cronUnescape is crontab(5)'s
// processing of the command field.
func TestLineRoundTripsThroughTheShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, cmd := range []string{
		`echo 50%`, `printf '%s\n' "a b"`, `echo it's`, `echo \%`, `echo "\\%" '\%' %%`, `echo a # comment`,
		`exit 3`, `echo $HOME`, `echo ")"`, `echo '`,
	} {
		l, err := Job{ID: 1, Schedule: "* * * * *", Command: cmd}.Line()
		if err != nil {
			t.Fatal(err)
		}
		field := cronUnescape(strings.SplitN(l, " ", 6)[5])
		// Replace the wrapper by one that prints its first argument.
		probe := strings.Replace(field, "/bin/sh -c "+Quote(wrapper), `/bin/sh -c 'printf "%s" "$1"'`, 1)
		out, err := exec.Command("sh", "-c", probe).Output()
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if string(out) != cmd {
			t.Errorf("the job received %q, want %q", out, cmd)
		}
	}
}

// cronUnescape is what Debian's cron does to the command field: "\%"
// becomes "%", and a bare "%" ends the command.
func cronUnescape(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		if esc {
			esc = false
			if r != '%' {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		if r == '%' {
			break
		}
		b.WriteRune(r)
	}
	if esc {
		b.WriteRune('\\')
	}
	return b.String()
}

func TestLineLength(t *testing.T) {
	base, _ := Job{ID: 1, Schedule: "* * * * *", Command: "x"}.Line()
	room := MaxCommandField - (len(base) - len("* * * * * ")) + 1
	if _, err := (Job{ID: 1, Schedule: "* * * * *", Command: strings.Repeat("x", room)}).Line(); err != nil {
		t.Fatalf("a command that fits was refused: %v", err)
	}
	_, err := Job{ID: 1, Schedule: "* * * * *", Command: strings.Repeat("x", room+1)}.Line()
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Msg, "too long") {
		t.Fatalf("err = %v", err)
	}
	if room < 600 {
		t.Fatalf("only %d bytes are left for the command", room)
	}
	if _, err := (Job{ID: 0, Schedule: "* * * * *", Command: "x"}).Line(); err == nil {
		t.Fatal("job id 0 accepted")
	}
}
