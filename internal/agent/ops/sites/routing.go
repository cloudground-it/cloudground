package sites

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// A site's routing block (SPEC §8, internal/engine SPEC §7).

// Routing is the site's routing as the editor shows it.
func (o *Ops) Routing(ctx context.Context, p rpc.SiteRef) (rpc.RoutingView, error) {
	v, err := o.Renderer.Routing(o.input(ctx, p.Site))
	if err != nil {
		return rpc.RoutingView{}, routingRefusal(err)
	}
	return rpc.RoutingView{Default: v.Default, Current: v.Current, Before: v.Before, After: v.After}, nil
}

// SetRouting renders the site with a new routing block and loads it. nginx
// tests it before the reload; when the test fails the render puts every
// file back and reloads nginx on them (SPEC §4.6), so the site goes on
// serving what it served.
func (o *Ops) SetRouting(ctx context.Context, p rpc.RoutingParams) (rpc.RoutingResult, error) {
	s := p.Site
	// The default is the type's: read without the stored block, so a
	// stored block that no longer passes can still be replaced.
	s.Config.Routing = ""
	view, err := o.Renderer.Routing(o.input(ctx, s))
	if err != nil {
		return rpc.RoutingResult{}, routingRefusal(err)
	}
	if view.Default == "" {
		return rpc.RoutingResult{}, agentrpc.Invalid("this site has no routing to edit")
	}
	routing := nginx.NormalizeRouting(p.Routing)
	if strings.TrimSpace(routing) == "" || routing == view.Default {
		routing = ""
	}
	s.Config.Routing = routing
	if routing != "" {
		// With this machine's listening ports, addresses and names.
		if err := nginx.CheckRouting(routing, nginx.RoutingContextOf(o.input(ctx, s))); err != nil {
			return rpc.RoutingResult{}, routingRefusal(err)
		}
	}
	files, err := o.render(ctx, s, true)
	if err != nil {
		var ce *host.ConfigError
		if errors.As(err, &ce) {
			return rpc.RoutingResult{}, agentrpc.Invalid("nginx refused the routing, nothing was changed: %s",
				nginxComplaint(ce.Output, o.vhostPath(s), strings.Count(view.Before, "\n"), strings.Count(routing, "\n")+1))
		}
		return rpc.RoutingResult{}, err
	}
	return rpc.RoutingResult{Routing: routing, Files: files}, nil
}

// routingRefusal shows a rule's refusal to the user; anything else stays an
// internal error.
func routingRefusal(err error) error {
	var re *nginx.RoutingError
	if errors.As(err, &re) {
		return agentrpc.Invalid("%s", re.Error())
	}
	return err
}

// emergRe is an error line of `nginx -t`: "2026/10/10 02:10:42 [emerg]
// 1297#1297: <message>", or "nginx: [emerg] <message>".
var emergRe = regexp.MustCompile(`\[emerg\] (?:\d+#\d+: )?(.*)`)

// nginxComplaint is the first error of `nginx -t`, with a line of the site's
// vhost given as the line of the routing block when it falls in it
// (blockStart lines of the vhost come before the block, which has
// blockLines).
func nginxComplaint(output, vhost string, blockStart, blockLines int) string {
	msg := strings.TrimSpace(output)
	for _, l := range strings.Split(output, "\n") {
		if m := emergRe.FindStringSubmatch(l); m != nil {
			msg = m[1]
			break
		}
	}
	if i := strings.LastIndex(msg, " in "+vhost+":"); i >= 0 {
		if n, err := strconv.Atoi(msg[i+len(" in "+vhost+":"):]); err == nil && n > blockStart && n <= blockStart+blockLines {
			return fmt.Sprintf("%s (routing line %d)", msg[:i], n-blockStart)
		}
	}
	return msg
}
