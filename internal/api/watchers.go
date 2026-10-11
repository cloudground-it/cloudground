package api

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Watchers turn state that is computed or read from the system, rather than
// stored, into events (internal/api/events/SPEC.md §4).

const defaultMonitorEvery = 15 * time.Second

// startWatchers runs the alert watcher (alert_service.go) and the live
// monitor until ctx ends.
func (s *Server) startWatchers(ctx context.Context) {
	s.Init()
	go s.watchAlerts(ctx)
	go s.liveMonitor(ctx)
}

// liveMonitor reads the server's status and its services from the agent
// while someone with a server-wide view has a stream open: one read for
// every browser, where each open dashboard used to ask every 15 s itself.
func (s *Server) liveMonitor(ctx context.Context) {
	every := s.monitorEvery
	if every <= 0 {
		every = defaultMonitorEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var last map[string]string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.monitorKick:
		}
		if s.globalStreams.Load() == 0 {
			last = nil // nobody watched: the next reading is a new baseline
			continue
		}
		var st rpc.SystemStatusResult
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.SystemStatus, agentrpc.None{}, &st); err == nil {
			s.Events.Publish(events.SystemStatus, 0, st)
		}
		var svc rpc.ServiceStatusResult
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.ServiceStatus, agentrpc.None{}, &svc); err != nil {
			continue
		}
		cur := serviceFingerprints(svc.Services)
		if last != nil {
			if changed := changedServices(last, cur); len(changed) > 0 {
				s.Events.Publish(events.ServiceChanged, 0, map[string]any{"names": changed})
			}
		}
		last = cur
	}
}

// kickMonitor asks the live monitor for a reading now.
func (s *Server) kickMonitor() {
	select {
	case s.monitorKick <- struct{}{}:
	default:
	}
}

// serviceFingerprints keeps what makes a service's row different to an
// operator: its state and when it last started (a restart), not its memory.
func serviceFingerprints(list []rpc.ServiceInfo) map[string]string {
	out := make(map[string]string, len(list))
	for _, sv := range list {
		out[sv.Name] = strings.Join([]string{sv.State, boolStr(sv.Active), boolStr(sv.Enabled), sv.Since.UTC().Format(time.RFC3339Nano)}, "|")
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func changedServices(prev, cur map[string]string) []string {
	var out []string
	for name, fp := range cur {
		if prev[name] != fp {
			out = append(out, name)
		}
	}
	for name := range prev {
		if _, ok := cur[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
