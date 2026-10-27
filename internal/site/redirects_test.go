package site

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func rule(match, from, to string, status int) state.RedirectRule {
	return state.RedirectRule{Match: match, From: from, To: to, Status: status}
}

func TestCheckRedirects(t *testing.T) {
	names := []string{"example.com", "www.example.com"}
	ok := []state.SiteRedirects{
		{},
		{Canonical: "www.example.com"},
		{Rules: []state.RedirectRule{rule("exact", "/old", "/new", 301)}},
		{Rules: []state.RedirectRule{rule("exact", "/", "/home.html", 302)}},
		{Rules: []state.RedirectRule{rule("exact", "/shop/", "/store/?src=old&x=%20y", 302)}},
		{Rules: []state.RedirectRule{rule("prefix", "/blog", "/news", 301)}},
		{Rules: []state.RedirectRule{rule("prefix", "/blog", "/", 301)}},
		{Rules: []state.RedirectRule{rule("prefix", "/a.b+c(d)", "https://docs.example.org:8443/v1", 302)}},
		{Rules: []state.RedirectRule{rule("exact", "/x", "https://example.org", 301)}},
		{Rules: []state.RedirectRule{rule("exact", "/blog", "/a", 301), rule("exact", "/blog/", "/b", 301)}},
	}
	for i, r := range ok {
		if err := CheckRedirects(r, names); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]state.SiteRedirects{
		"canonical not a name": {Canonical: "other.com"},
		"unknown match":        {Rules: []state.RedirectRule{rule("regex", "/a", "/b", 301)}},
		"status 307":           {Rules: []state.RedirectRule{rule("exact", "/a", "/b", 307)}},
		"no leading slash":     {Rules: []state.RedirectRule{rule("exact", "a", "/b", 301)}},
		"space in path":        {Rules: []state.RedirectRule{rule("exact", "/a b", "/b", 301)}},
		"semicolon":            {Rules: []state.RedirectRule{rule("exact", "/a;b", "/b", 301)}},
		"brace in path":        {Rules: []state.RedirectRule{rule("exact", "/a{", "/b", 301)}},
		"dollar in path":       {Rules: []state.RedirectRule{rule("exact", "/a$uri", "/b", 301)}},
		"quote in path":        {Rules: []state.RedirectRule{rule("exact", `/a"`, "/b", 301)}},
		"backslash in path":    {Rules: []state.RedirectRule{rule("exact", `/a\`, "/b", 301)}},
		"hash in path":         {Rules: []state.RedirectRule{rule("exact", "/a#x", "/b", 301)}},
		"percent in path":      {Rules: []state.RedirectRule{rule("exact", "/a%20b", "/b", 301)}},
		"dot segment":          {Rules: []state.RedirectRule{rule("exact", "/a/../b", "/c", 301)}},
		"double slash":         {Rules: []state.RedirectRule{rule("exact", "/a//b", "/c", 301)}},
		"newline in path":      {Rules: []state.RedirectRule{rule("exact", "/a\nb", "/c", 301)}},
		"prefix of the root":   {Rules: []state.RedirectRule{rule("prefix", "/", "/b", 301)}},
		"prefix trailing /":    {Rules: []state.RedirectRule{rule("prefix", "/a/", "/b", 301)}},
		"empty target":         {Rules: []state.RedirectRule{rule("exact", "/a", "", 301)}},
		"target with space":    {Rules: []state.RedirectRule{rule("exact", "/a", "/b c", 301)}},
		"target semicolon":     {Rules: []state.RedirectRule{rule("exact", "/a", "/b; return 200", 301)}},
		"target dollar":        {Rules: []state.RedirectRule{rule("exact", "/a", "/b$host", 301)}},
		"target brace":         {Rules: []state.RedirectRule{rule("exact", "/a", "/b}", 301)}},
		"target quote":         {Rules: []state.RedirectRule{rule("exact", "/a", `/b"`, 301)}},
		"target newline":       {Rules: []state.RedirectRule{rule("exact", "/a", "/b\nreturn 200", 301)}},
		"protocol relative":    {Rules: []state.RedirectRule{rule("exact", "/a", "//evil.test/x", 301)}},
		"bad percent":          {Rules: []state.RedirectRule{rule("exact", "/a", "/b%zz", 301)}},
		"javascript scheme":    {Rules: []state.RedirectRule{rule("exact", "/a", "javascript:alert(1)", 301)}},
		"ftp scheme":           {Rules: []state.RedirectRule{rule("exact", "/a", "ftp://example.org/", 301)}},
		"userinfo":             {Rules: []state.RedirectRule{rule("exact", "/a", "https://user@example.org/", 301)}},
		"bad host":             {Rules: []state.RedirectRule{rule("exact", "/a", "https://-bad-/x", 301)}},
		"port 0":               {Rules: []state.RedirectRule{rule("exact", "/a", "https://example.org:0/", 301)}},
		"port too big":         {Rules: []state.RedirectRule{rule("exact", "/a", "https://example.org:99999/", 301)}},
		"prefix target query":  {Rules: []state.RedirectRule{rule("prefix", "/a", "/b?x=1", 301)}},
		"loop exact":           {Rules: []state.RedirectRule{rule("exact", "/a", "/a", 301)}},
		"loop exact query":     {Rules: []state.RedirectRule{rule("exact", "/a", "/a?x=1", 301)}},
		"loop prefix":          {Rules: []state.RedirectRule{rule("prefix", "/a", "/a/b", 301)}},
		"duplicate":            {Rules: []state.RedirectRule{rule("exact", "/a", "/b", 301), rule("exact", "/a", "/c", 302)}},
		"prefix and exact":     {Rules: []state.RedirectRule{rule("exact", "/a", "/b", 301), rule("prefix", "/a", "/c", 302)}},
		"too long a path":      {Rules: []state.RedirectRule{rule("exact", "/"+strings.Repeat("a", MaxRedirectFrom), "/b", 301)}},
		"too long a target":    {Rules: []state.RedirectRule{rule("exact", "/a", "/"+strings.Repeat("b", MaxRedirectTarget), 301)}},
	}
	for name, r := range bad {
		if err := CheckRedirects(r, names); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
	many := state.SiteRedirects{}
	for i := 0; i <= MaxRedirectRules; i++ {
		many.Rules = append(many.Rules, rule("exact", fmt.Sprintf("/p%d", i), "/t", 301))
	}
	if err := CheckRedirects(many, names); err == nil {
		t.Error("more than the limit was accepted")
	}
}

func TestNormalizeRedirects(t *testing.T) {
	got := NormalizeRedirects(state.SiteRedirects{Canonical: "example.com", Rules: []state.RedirectRule{
		rule("prefix", "/blog/", "/news/", 301), rule("prefix", "/x", "/", 302), rule("exact", "/shop/", "/store/", 301),
	}})
	want := []state.RedirectRule{rule("prefix", "/blog", "/news", 301), rule("prefix", "/x", "/", 302), rule("exact", "/shop/", "/store/", 301)}
	if got.Canonical != "example.com" || len(got.Rules) != 3 {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got.Rules[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got.Rules[i], want[i])
		}
	}
}
