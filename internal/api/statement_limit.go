package api

import "sync"

// The database statements a person runs from the panel (the console, DB
// Studio's SQL editor, rows, edits and ALTERs) run while the request waits,
// each up to a minute on the server. Without a bound, one account could keep
// the site's database (and the server's) busy with as many as it could
// send at once (SR-T5).
const (
	// maxStatementsPerUser: an account's statements at once, over every
	// site and every tab.
	maxStatementsPerUser = 2
	// maxStatementsPerSite: a site's statements at once, from every
	// account.
	maxStatementsPerSite = 4
)

// msgTooManyStatements is the 429's message.
const msgTooManyStatements = "too many database statements are running: wait for one to finish"

// statementLimiter counts the statements running per account and per site.
// The zero value is ready.
type statementLimiter struct {
	mu     sync.Mutex
	byUser map[string]int
	bySite map[int64]int
}

// acquire reserves a statement for user (call.statementKey: an account, or
// a database user signed in to Quarry) on site: a release func, or false
// when either is at its limit (nothing is reserved then).
func (l *statementLimiter) acquire(user string, site int64) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser, l.bySite = map[string]int{}, map[int64]int{}
	}
	if l.byUser[user] >= maxStatementsPerUser || l.bySite[site] >= maxStatementsPerSite {
		return nil, false
	}
	l.byUser[user]++
	l.bySite[site]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.byUser[user]--; l.byUser[user] <= 0 {
				delete(l.byUser, user)
			}
			if l.bySite[site]--; l.bySite[site] <= 0 {
				delete(l.bySite, site)
			}
		})
	}, true
}
