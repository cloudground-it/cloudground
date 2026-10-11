# ops/certs — site certificates

Issuing a Let's Encrypt certificate for a site, installing or removing one the
operator brings, reporting expiry, and cleaning up when a site is deleted.

Sources: the e2e suite (`e2e/features_test.go`: custom certificate served and
removed, the certificate/delete race R-0.4, the custom directory removed on
delete R-1.2, the certificate status route), the API callers
(`internal/api/sites.go` issue, custom, status and the HSTS guard;
`internal/api/scheduler.go` renewal), `docs/operations.md` "Certificates",
the installer (ACME webroot, certbot deploy hook).

Everything goes through `host.ACME` (certbot). RPC names, parameters and
results are those of `internal/agentrpc/methods.go`; `rpc.CertInfo` gains
one field, `custom` (§3).

## 1. Locks

- The site lock is the RPC server's (`LockLiveSite` in the method table) for
  `IssueCertificate` and `CustomCertificate`: a site deleted while the call
  waited is refused with `conflict`, and its vhost is never rendered back.
- `CertMu` (shared with the panel certificate and site deletion) is held
  **only around certbot**: certbot allows one process against its state. It
  is not held while the site is re-rendered, and a custom certificate never
  takes it (it does not run certbot; the site lock already orders it against
  that site's deletion). The old code held it across certbot and the render,
  and made a custom upload wait for any other site's certbot run.
- Order everywhere: site lock → `CertMu` → nginx lock (the render takes the
  nginx lock itself).

## 2. IssueCertificate

Params `{site, email, staging, wildcard, cloudflare_token}`.

1. `email` empty → `invalid` "acme account email required"; not an address
   (`net/mail`), or starting with `-` → `invalid` "invalid acme account email".
2. **Wildcard** (`wildcard: true`): DNS-01 at Cloudflare for `<domain>` and
   `*.<domain>`. The token must look like one (`[A-Za-z0-9_-]{30,80}`), else
   `invalid`. It is written to `<certbot dir>/cloudground-cloudflare.ini`
   (`dns_cloudflare_api_token = …`, 0600 root, atomic replace), the file
   certbot's renewal configuration points at. Aliases are not requested.
3. **HTTP-01** otherwise, for `<domain>` and its aliases, through the ACME
   webroot. Before certbot spends a rate-limited attempt, a **preflight**
   proves each name reaches this server's webroot (§2.1).
4. `ACME.CertOnly{Name: domain, Domains, Email, Webroot | CloudflareCredentials,
   Staging}` under `CertMu`. A certbot failure is `internal` ("Could not issue
   the certificate"); certbot's stderr goes to the log detail only.
5. The site is re-rendered (the vhost now names the issued certificate) and
   the managed files are returned: `ApplyResult{files}`.

### 2.1 Preflight

- A random token `cloudground-<32 hex>` is written to
  `<webroot>/.well-known/acme-challenge/<token>`, readable by nginx: the two
  directories (and the webroot, when the agent has to make it) are created
  0755 and the file 0644 **explicitly**, whatever the
  agent's umask (`UMask=0027` once made every validation answer 403). The file
  is opened `O_EXCL|O_NOFOLLOW` and removed when the preflight ends.
- For each name: `GET http://<name>/.well-known/acme-challenge/<token>`,
  12 s timeout, redirects not followed. Anything but `200` with exactly the
  token as body → `invalid`, naming the domain and what its DNS resolves to
  ("DNS does not resolve" / "DNS resolves to a, b"), plus the advice to point
  A/AAAA here and stop proxies intercepting the challenge path.

## 3. CertStatus

Params `{names}`; result `{certs: name → {exists, not_after, issuer, custom}}`.

- A name is a domain (`site.ValidateDomain`) or the panel's lineage
  `cloudground-panel`; anything else → `invalid`, before any file is read.
- Read, in order of precedence:
  - `cloudground-panel` → **the installed panel certificate** (`PanelCert`,
    `/etc/cloudground/certs/panel.pem`), the file nginx serves for the panel
    domain. The old code read the certbot lineage. certbot's own timer renews
    that lineage, so it was fresh while the copy nginx serves aged. The
    scheduler then never asked for a re-install, and the panel's certificate
    expired 90 days after issuance.
  - a domain with `<CustomCertDir>/<domain>/fullchain.pem` → that file,
    `custom: true` (it is what the vhost serves, `engine` input);
  - otherwise `<CertLiveDir>/<domain>/fullchain.pem`.
- The first PEM block is parsed with `crypto/x509` (no openssl):
  `not_after` = NotAfter, `issuer` = issuer CN. Missing, not PEM, or not a
  certificate → `{exists: false}` for that name, not an error.
- The API's renewal pass skips `custom` certificates: renewing one with
  certbot obtained a Let's Encrypt certificate the vhost never serves (the
  custom one takes precedence), every 12 hours, and logged a failure each time
  the domain did not answer HTTP-01 here (a Cloudflare origin certificate).

## 4. CustomCertificate

Params `{site, fullchain, key, remove}`.

- `remove: true` → `<CustomCertDir>/<domain>` is removed (§5's removal),
  then the site is re-rendered: nginx goes back to Let's Encrypt or the
  fallback certificate.
- Otherwise:
  - `fullchain` and `key` must form a pair (`tls.X509KeyPair`), else `invalid`
    "the certificate and key do not form a pair";
  - the leaf must cover the domain (`VerifyHostname`), else `invalid` "the
    certificate does not cover <domain>";
  - it must not be expired, else `invalid` "the certificate expired on
    <date>".
- Then `<CustomCertDir>/<domain>/` (0700) receives `privkey.pem` (0600) and
  `fullchain.pem` (0644), each written by atomic replace (temp, fsync,
  rename). The site is re-rendered and its files returned.

## 5. On site deletion (`RemoveSite`)

Called by the site deletion with the site lock held:

- under `CertMu`, `ACME.Delete(domain)`. A lineage that does not exist
  (`host.ErrNoSuchCert`, decided from certbot's renewal directory, not its
  output) is not an error; another failure is logged and the deletion goes on.
- `<CustomCertDir>/<domain>` is removed through descriptors that never follow
  a symlink: a symlink planted at the name is unlinked, never followed. A
  name that is not a domain is refused. A missing directory is fine. A failure
  is returned (the deletion reports itself incomplete).

## 6. Not here

The panel's own certificate and self-update are `ops/panel`. Rendering a
site's vhost belongs to the site domain; this package calls it through
`Ops.Render`.
