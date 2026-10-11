package sites

import (
	"bufio"
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// What a site's routing block must not reach (internal/engine SPEC §7.3),
// read from this machine at every render: a service started after the
// routing was saved is covered at the next render.

// systemPorts are the TCP ports s's routing may not reach: every port a
// process listens on that is not s's own user's (its shell login shares
// the uid). Another site's app server is another tenant's (R-10.5), as
// reserved as a system service's. An owner that cannot be read reserves
// every listening port.
func (o *Ops) systemPorts(s state.Site) []int {
	if o.SystemPorts != nil {
		return o.SystemPorts()
	}
	uid := -1
	if o.Owner != nil {
		if ow, err := o.Owner(s.SystemUser); err == nil {
			uid = ow.UID
		}
	}
	return listeningPorts("/proc", ownedBy(uid))
}

// ownedBy is the listener filter of one site: only uid's own sockets are
// the site's.
func ownedBy(uid int) func(int) bool {
	return func(u int) bool { return uid >= 0 && u == uid }
}

func (o *Ops) localAddrs() []netip.Addr {
	if o.LocalAddrs != nil {
		return o.LocalAddrs()
	}
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	return out
}

// resolve looks a proxy host name up, bounded: a render does not wait on
// DNS for long.
func (o *Ops) resolve(host string) ([]netip.Addr, error) {
	if o.Resolve != nil {
		return o.Resolve(host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	return ips, err
}

// listeningPorts reads <proc>/net/tcp and tcp6 for sockets in LISTEN whose
// owner isSite does not claim.
func listeningPorts(proc string, isSite func(uid int) bool) []int {
	var ports []int
	for _, f := range []string{"tcp", "tcp6"} {
		file, err := os.Open(filepath.Join(proc, "net", f))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(file)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid …
			if len(fields) < 8 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			port, err1 := strconv.ParseInt(portHex, 16, 32)
			uid, err2 := strconv.Atoi(fields[7])
			if !ok || err1 != nil || err2 != nil || isSite(uid) || slices.Contains(ports, int(port)) {
				continue
			}
			ports = append(ports, int(port))
		}
		file.Close()
	}
	slices.Sort(ports)
	return ports
}
