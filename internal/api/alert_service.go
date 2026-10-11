package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Alerts (SPEC.md §11): rows of the store's alerts table, one per condition.
// The evaluator derives the conditions and syncs each kind's set; the
// dashboard reads the table; the webhook sends what is new.

// Alert is one alert as GET /api/system/alerts answers it.
type Alert struct {
	ID      string `json:"id"`
	Level   string `json:"level"` // warning | error
	Kind    string `json:"kind"`  // certificate | task | service | drift | deploy
	SiteID  int64  `json:"site_id,omitempty"`
	Site    string `json:"site,omitempty"`
	Message string `json:"message"`
	// Text is Message in the reader's language.
	Text string `json:"text,omitempty"`
	At   string `json:"at,omitempty"`
	// TaskKind, TaskName and Detail, on a task alert: the failed job's
	// kind ("backup.run") for machines, its name in the reader's language
	// (i18n.TaskName, the names the task list uses), and its error, in the
	// reader's language when the catalogue has it. The interface parsed
	// these out of the English Message, so a task alert read half in
	// English whatever the language.
	TaskKind string `json:"task_kind,omitempty"`
	TaskName string `json:"task_name,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

const (
	// certWarnWindow is when a certificate's coming expiry becomes an
	// alert: renewal starts 30 days out, so 14 days left means several
	// renewals failed. Under certErrorWindow it is an error.
	certWarnWindow  = 14 * 24 * time.Hour
	certErrorWindow = 3 * 24 * time.Hour
	// failedTaskWindow is how long a failed task stays an alert.
	failedTaskWindow = 24 * time.Hour
	// notifyEvery is how often one alert may be sent to the webhook.
	notifyEvery = 6 * time.Hour
	// alertDebounce gathers the events of one change into one evaluation.
	alertDebounce = time.Second
)

// firstLine is s up to its first line break.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// certificateConditions are the certificate alerts of the active sites: a
// renewal that failed, an expiry inside the warning window. The expiry's
// fingerprint is its date and level, not the day count, so it keeps its id
// while it counts down and gets a new one when it escalates.
func certificateConditions(sites []state.Site, certs map[int64]state.CertState, now time.Time) []state.Alert {
	var out []state.Alert
	for _, site := range sites {
		if site.Status != state.SiteActive {
			continue
		}
		subject := ":site:" + strconv.FormatInt(site.ID, 10)
		cert := certs[site.ID]
		if cert.Error != "" {
			why := firstLine(cert.Error)
			out = append(out, state.Alert{Key: "certificate.renewal" + subject, SiteID: site.ID, Level: "error",
				Fingerprint: why, Message: "certificate renewal failed: " + why,
				Text: state.AlertText{Key: "alert.certRenewal", Args: []string{why}}})
		}
		left := cert.Expiry.Sub(now)
		if cert.Expiry.IsZero() || left >= certWarnWindow {
			continue
		}
		level := "warning"
		if left < certErrorWindow {
			level = "error"
		}
		expiry := cert.Expiry.UTC().Format(time.RFC3339)
		days := int(left.Hours() / 24)
		out = append(out, state.Alert{Key: "certificate.expiry" + subject, SiteID: site.ID, Level: level,
			Fingerprint: expiry + "|" + level, Message: fmt.Sprintf("certificate expires in %d days", days), At: expiry,
			Text: state.AlertText{Key: "alert.certExpires", Counted: true, Count: days}})
	}
	return out
}

// taskConditions are the tasks that failed inside the window. One of a site
// that no longer exists (a creation that failed and was rolled back) is a
// server-wide alert, named by the domain its job recorded: an admin must
// see a creation that failed.
func taskConditions(list []state.Job, exists map[int64]bool, now time.Time) []state.Alert {
	var out []state.Alert
	newest := map[string]time.Time{}
	since := now.Add(-failedTaskWindow)
	for _, j := range list {
		if j.Status != state.JobFailed || j.Quiet || j.CreatedAt.Before(since) {
			continue
		}
		why := firstLine(j.Error)
		site, subject := j.SiteID, ""
		if site != 0 && !exists[site] {
			site, subject = 0, jobDomain(j)
		}
		key, fingerprint := "task:"+strconv.FormatInt(j.ID, 10), ""
		if recurring[j.Kind] && j.SiteID != 0 {
			// One alert per site for a job the schedule repeats: the
			// newest failure, raised again only when its reason changes.
			key, fingerprint = "task:"+j.Kind+":site:"+strconv.FormatInt(j.SiteID, 10), why
			if prev, ok := newest[key]; ok && !j.CreatedAt.After(prev) {
				continue
			}
			newest[key] = j.CreatedAt
			out = slices.DeleteFunc(out, func(a state.Alert) bool { return a.Key == key })
		}
		out = append(out, state.Alert{Key: key, SiteID: site, Subject: subject, Level: "error", Fingerprint: fingerprint,
			Message: i18n.TaskName("en", j.Kind) + " failed: " + why, At: j.CreatedAt.UTC().Format(time.RFC3339),
			Text: state.AlertText{Key: "alert.taskFailed", Args: []string{j.Kind, why}, DetailKey: j.ErrorKey, DetailArgs: j.ErrorArgs}})
	}
	return out
}

// recurring are the job kinds a schedule queues again and again: their
// failures are one alert per site, not one per run.
var recurring = map[string]bool{"backup.scheduled": true}

// jobDomain is the domain of the site a job was about, as its payload
// recorded it (startJob), or "site #<id>" when it recorded none.
func jobDomain(j state.Job) string {
	var p jobRecord
	if json.Unmarshal([]byte(j.Payload), &p) == nil && p.Domain != "" {
		return p.Domain
	}
	return "site #" + strconv.FormatInt(j.SiteID, 10)
}

// serviceConditions are the services that failed or were stopped.
func serviceConditions(list []rpc.ServiceInfo) []state.Alert {
	var out []state.Alert
	for _, sv := range list {
		key := map[string]string{"failed": "alert.serviceFailed", "stopped": "alert.serviceStopped"}[sv.State]
		if key == "" {
			continue
		}
		out = append(out, state.Alert{Key: "service:" + sv.Name, Level: "error", Fingerprint: sv.State,
			Message: fmt.Sprintf("%s is %s: %s", sv.Name, sv.State, sv.Detail),
			Text:    state.AlertText{Key: key, Args: []string{sv.Name, sv.Detail}}})
	}
	return out
}

// driftConditions is one alert for the open drift, newest first. The newest
// event is its fingerprint: more drift brings a dismissed alert back, a
// file put right alone does not.
func driftConditions(open []state.DriftEvent) []state.Alert {
	if len(open) == 0 {
		return nil
	}
	n := len(open)
	return []state.Alert{{Key: "drift", Level: "warning", Fingerprint: strconv.FormatInt(open[0].ID, 10),
		Message: fmt.Sprintf("%d managed file(s) changed outside the panel", n),
		Text:    state.AlertText{Key: "alert.drift", Counted: true, Count: n}}}
}

// evaluateAlerts brings the alerts table to the conditions as they are now.
func (s *Server) evaluateAlerts(ctx context.Context) error {
	return s.evaluateAlertsAt(ctx, time.Now())
}

// evaluateAlertsAt is evaluateAlerts at the time now. A kind whose source
// cannot be read keeps the alerts it had; the first such error is returned.
// One evaluation runs at a time (the watcher's and the job's), so an older
// read can never clear what a newer one raised.
func (s *Server) evaluateAlertsAt(ctx context.Context, now time.Time) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	var first error
	keep := func(err error, what string) bool {
		if err != nil {
			s.logIfErr(err, what)
			if first == nil {
				first = err
			}
			return false
		}
		return true
	}
	sites, sitesErr := s.Store.ListSites()
	certs, certsErr := s.Store.CertStates()
	if keep(errors.Join(sitesErr, certsErr), "alerts: certificates") {
		keep(s.Store.SyncAlerts("certificate", certificateConditions(sites, certs, now)), "SyncAlerts certificate")
	}
	exists := make(map[int64]bool, len(sites))
	for _, site := range sites {
		exists[site.ID] = true
	}
	if list, err := s.Store.ListJobs(200, false); keep(err, "alerts: ListJobs") && sitesErr == nil {
		keep(s.Store.SyncAlerts("task", taskConditions(list, exists, now)), "SyncAlerts task")
	}
	guessing, guessed := s.guesses.condition(now)
	if svc, err := agentrpc.Call(ctx, s.Agent, agentrpc.ServiceStatus, agentrpc.None{}); keep(err, "alerts: ServiceStatus") {
		set := serviceConditions(svc.Services)
		if guessed {
			set = append(set, guessing)
		}
		keep(s.Store.SyncAlerts("service", set), "SyncAlerts service")
	} else if guessed {
		// The services cannot be read, so their alerts stay as they are;
		// token guessing is the panel's own to report.
		guessing.Kind = "service"
		keep(s.Store.RaiseAlert(guessing), "RaiseAlert token guessing")
	}
	if open, err := s.Store.ListDriftEvents(false); keep(err, "alerts: ListDriftEvents") {
		keep(s.Store.SyncAlerts("drift", driftConditions(open)), "SyncAlerts drift")
	}
	if noted, err := s.Store.NotedDeployments(); keep(err, "alerts: NotedDeployments") {
		keep(s.Store.SyncAlerts("deploy", deployConditions(noted)), "SyncAlerts deploy")
	}
	if !s.alertsSettled {
		// The first evaluation after the upgrade to the table has re-raised
		// what the old panel had announced; from now on alerts are new.
		if keep(s.Store.EndAlertsUpgrade(), "EndAlertsUpgrade") {
			s.alertsSettled = true
		}
	}
	return first
}

// alertTriggers are the events after which the watcher evaluates again.
var alertTriggers = map[events.Type]bool{
	events.TaskFinished: true, events.SiteChanged: true, events.SiteDeleted: true,
	events.CertChanged: true, events.DriftChanged: true, events.ServiceChanged: true,
}

// watchAlerts evaluates once, then again (at most once per alertDebounce)
// after every event that can change a condition, until ctx ends. The store
// publishes alert.raised and alert.cleared itself.
func (s *Server) watchAlerts(ctx context.Context) {
	_ = s.evaluateAlerts(ctx)
	sub, _, _ := s.Events.Subscribe("", 256)
	defer func() { sub.Close() }()
	var due <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C():
			if !ok {
				// Dropped: something was missed, so look again.
				sub, _, _ = s.Events.Subscribe("", 256)
			} else if !alertTriggers[ev.Type] {
				continue
			}
			if due == nil {
				due = time.After(alertDebounce)
			}
		case <-s.alertKick:
			if due == nil {
				due = time.After(alertDebounce)
			}
		case <-due:
			due = nil
			_ = s.evaluateAlerts(ctx)
		}
	}
}

// kickAlerts asks the watcher for an evaluation (a condition with no event
// of its own, as token guessing, has appeared).
func (s *Server) kickAlerts() {
	select {
	case s.alertKick <- struct{}{}:
	default:
	}
}

// runAlertsNotify is the alerts.notify job: the time-based conditions (a
// certificate entering its window, a task leaving its day) have no event of
// their own, so the job evaluates, then sends the webhook.
func (s *Server) runAlertsNotify(ctx context.Context, r *jobs.Run) error {
	err := s.evaluateAlerts(ctx)
	s.notifyAlerts(ctx)
	return err
}

// canSeeAlert reports whether the caller may see an alert about siteID: an
// operator only its own sites' and never the server's.
func canSeeAlert(c *call, siteID int64) bool {
	if siteID == 0 {
		return c.User.Role != roleOperator
	}
	return c.CanAccess(siteID)
}

// alertMessage is the alert's text in lang: its catalogue key with its
// arguments, a task's error in its own translation when the catalogue has
// it, the English message when there is no key.
func alertMessage(a state.Alert, lang string) string {
	t := a.Text
	if t.Key == "" {
		return a.Message
	}
	args := make([]any, len(t.Args))
	for i, v := range t.Args {
		args[i] = v
	}
	if n := len(args); n > 0 {
		if msg, ok := localized(lang, t.DetailKey, t.DetailArgs); ok {
			args[n-1] = firstLine(msg)
		}
	}
	if kind := alertTaskKind(a); kind != "" {
		args[0] = i18n.TaskName(lang, kind)
	}
	if t.Counted {
		return i18n.N(lang, t.Key, t.Count, args...)
	}
	return i18n.T(lang, t.Key, args...)
}

// alertTaskKind is the failed job's kind of a task alert, "" for any other.
// An alert raised before task names (#13) stored the kind with its dots as
// spaces ("backup scheduled"); no kind has a space, so both read back.
func alertTaskKind(a state.Alert) string {
	if t := a.Text; t.Key == "alert.taskFailed" && len(t.Args) == 2 {
		return strings.ReplaceAll(t.Args[0], " ", ".")
	}
	return ""
}

// alertView is a as the dashboard gets it, written for lang.
func alertView(a state.Alert, lang string) Alert {
	v := Alert{ID: a.ID, Level: a.Level, Kind: a.Kind, SiteID: a.SiteID, Site: a.Site,
		Message: a.Message, Text: alertMessage(a, lang), At: a.At}
	if kind := alertTaskKind(a); kind != "" {
		t := a.Text
		v.TaskKind, v.TaskName = kind, i18n.TaskName(lang, kind)
		v.Detail = t.Args[1]
		if msg, ok := localized(lang, t.DetailKey, t.DetailArgs); ok {
			v.Detail = firstLine(msg)
		}
	}
	return v
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.Store.ListAlerts()
	if err != nil {
		s.logIfErr(err, "ListAlerts")
		respondErr(w, http.StatusInternalServerError, "could not read the alerts")
		return
	}
	lang := readerLanguage(r, c)
	out := []Alert{}
	for _, a := range list {
		if canSeeAlert(c, a.SiteID) {
			out = append(out, alertView(a, lang))
		}
	}
	respond(w, http.StatusOK, out)
}

// dismissAlert hides an alert the caller can see, for everyone, for as long
// as its occurrence lasts.
func (s *Server) dismissAlert(c *call, id string) (state.Alert, error) {
	missing := decline(http.StatusNotFound, "alert not found")
	a, err := s.Store.GetAlert(id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return a, missing
	case err != nil:
		s.logIfErr(err, "GetAlert")
		return a, decline(http.StatusInternalServerError, "could not read the alerts")
	case a.DismissedAt != nil || !canSeeAlert(c, a.SiteID):
		return a, missing
	}
	switch err := s.Store.DismissAlert(id); {
	case errors.Is(err, state.ErrNotFound):
		return a, missing
	case err != nil:
		s.logIfErr(err, "DismissAlert")
		return a, decline(http.StatusInternalServerError, "could not dismiss the alert")
	}
	return a, nil
}

func (s *Server) handleDismissAlert(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	a, err := s.dismissAlert(c, id)
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(c.Actor(), "alert.dismiss", a.Site, a.Kind+": "+firstLine(a.Message))
	respond(w, http.StatusOK, map[string]string{"dismissed": id})
}

// webhookPosts is how many webhook posts run at once.
const webhookPosts = 4

// notifyAlerts sends every alert not sent in the last notifyEvery to the
// webhook, if one is set, in the admins' language. Slack, Discord,
// Mattermost and most chat tools take a JSON body with "text" (or
// "content"). Posts run webhookPosts at a time and each alert is marked
// as soon as its post succeeded, so a hanging endpoint costs one timeout per
// round and a failed post is tried again at the next run.
func (s *Server) notifyAlerts(ctx context.Context) {
	hook, err := s.Store.Secret(state.SecretNotifyWebhook)
	s.logIfErr(err, "Secret")
	if hook == "" {
		return
	}
	due, err := s.Store.AlertsToNotify(time.Now().Add(-notifyEvery))
	if err != nil || len(due) == 0 {
		s.logIfErr(err, "AlertsToNotify")
		return
	}
	lang, client := s.notifyLocale(), s.webhookHTTP()
	slots := make(chan struct{}, webhookPosts)
	var wg sync.WaitGroup
	for _, a := range due {
		wg.Add(1)
		slots <- struct{}{}
		go func(a state.Alert) {
			defer func() { <-slots; wg.Done() }()
			if err := postWebhook(ctx, client, hook, webhookBody(a, lang)); err != nil {
				// err never carries the URL: it is a credential, and the
				// panel's journal is readable by read-only accounts.
				if s.Log != nil {
					s.Log.Warn("alert webhook failed", "alert", a.ID, "err", err)
				}
				return
			}
			s.logIfErr(s.Store.MarkAlertsNotified([]string{a.ID}), "MarkAlertsNotified")
		}(a)
	}
	wg.Wait()
}

// alertText is the webhook's line for a: its level, its site and its text.
func alertText(a state.Alert, lang string) string {
	level, msg := i18n.T(lang, "alert.level."+a.Level), alertMessage(a, lang)
	if a.Site == "" {
		return i18n.T(lang, "alert.header", level, msg)
	}
	return i18n.T(lang, "alert.headerSite", level, a.Site, msg)
}

// webhookAlert is the part of a webhook's body for machines: the alert's
// raw fields, in no language, beside the chat tools' "text" and "content".
// Nested, so no field can clash with one a chat tool reads.
type webhookAlert struct {
	ID       string `json:"id"`
	Level    string `json:"level"`
	Kind     string `json:"kind"`
	Site     string `json:"site,omitempty"`
	TaskKind string `json:"task_kind,omitempty"`
	At       string `json:"at,omitempty"`
}

// webhookBody is the JSON the webhook gets for a, written for lang.
func webhookBody(a state.Alert, lang string) map[string]any {
	text := alertText(a, lang)
	return map[string]any{"text": text, "content": text, "cloudground": webhookAlert{
		ID: a.ID, Level: a.Level, Kind: a.Kind, Site: a.Site, TaskKind: alertTaskKind(a), At: a.At}}
}

// notifyLocale is the webhook's language. The webhook is one channel for
// the whole panel, read by its admins: it speaks the language of the first
// enabled admin (by creation, normally the account made at setup) who chose
// one, and the default otherwise.
func (s *Server) notifyLocale() string {
	users, err := s.Store.ListUsers()
	s.logIfErr(err, "ListUsers")
	for _, u := range users {
		if u.Role == roleAdmin && !u.Disabled && i18n.Supported(u.Locale) {
			return u.Locale
		}
	}
	return i18n.Default
}

// postWebhook posts one body to the webhook, within 10 s. Its error never
// names the URL (an *url.Error would): only the status or the cause.
func postWebhook(ctx context.Context, client *http.Client, target string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("the webhook URL is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return errors.New("the webhook could not be reached")
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errors.New("the webhook answered " + resp.Status)
	}
	return nil
}
