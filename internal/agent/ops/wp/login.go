package wp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// loginTTL is how long a login link stays usable.
const loginTTL = 5 * time.Minute

// loginOption prefixes the option row that holds a link's hash
// (connector/SPEC.md §2.5).
const loginOption = "cloudground_login_"

// MagicLogin issues a one-shot sign-in link for the site's first
// administrator (SPEC §2). Only the token's hash is stored, as the name of
// an option row, which the connector reads by that name and deletes.
func (o *Ops) MagicLogin(ctx context.Context, p rpc.WPParams) (rpc.WPLoginResult, error) {
	out, err := o.Run(ctx, p.Site, "user", "list", "--role=administrator", "--field=ID",
		"--orderby=ID", "--order=ASC", "--number=1")
	if err != nil {
		return rpc.WPLoginResult{}, fmt.Errorf("list administrators: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return rpc.WPLoginResult{}, agentrpc.Conflict("no administrator user found")
	}

	// One live link per site: an unused earlier one stops working.
	out, err = o.Run(ctx, p.Site, "option", "list", "--search="+loginOption+"*", "--field=option_name")
	if err != nil {
		return rpc.WPLoginResult{}, fmt.Errorf("list earlier login links: %w", err)
	}
	var stale []string
	for _, name := range strings.Fields(string(out)) {
		if rest, ok := strings.CutPrefix(name, loginOption); ok && isHex(rest) {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		if _, err := o.Run(ctx, p.Site, append([]string{"option", "delete"}, stale...)...); err != nil {
			return rpc.WPLoginResult{}, fmt.Errorf("remove earlier login links: %w", err)
		}
	}

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return rpc.WPLoginResult{}, err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	value := id + ":" + strconv.FormatInt(o.now().Add(loginTTL).Unix(), 10)
	if _, err := o.Run(ctx, p.Site, "option", "add", loginOption+hex.EncodeToString(sum[:]), value, "--autoload=no"); err != nil {
		return rpc.WPLoginResult{}, fmt.Errorf("store the login token: %w", err)
	}
	return rpc.WPLoginResult{URL: "https://" + p.Site.Domain + "/?cloudground_login=1#" + token}, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == 64
}
