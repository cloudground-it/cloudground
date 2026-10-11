# `internal/agent/ops/staging` — staging copies (specification)

A staging site is an ordinary site (`sites` row, Unix user, tree, database,
PHP pool, vhost) whose `StagingOf` names its production site. This package
clones production into it, compares the two trees, lists the staging
database's tables with the push policy applied, and pushes selected tables
back to production. The API owns the records, the secrets and the safety
snapshot taken before a push (Phase 0, R-0.10); the agent owns everything that
touches the two sites, and the policy that says which tables may never be
pushed.

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `CreateStaging` | `StagingParams{Production, Staging, DBPassword, ConnectorToken}` → `CreateSiteResult` | Sites, locks itself, 2 h |
| `StagingDiff` | `StagingParams{Production, Staging}` → `StagingDiffResult` | Sites, read-only, 15 min |
| `StagingTables` | `StagingParams{Production, Staging}` → `StagingTablesResult{Tables: [{Name, Protected, Reason}]}` | Sites, read-only, 2 min |
| `SyncStagingDatabase` | `SyncStagingDBParams{Production, Staging, Tables}` → `{"tables", "count"}` | Sites, locks itself, 2 h |

Every method refuses (`invalid`) a pair where `Staging.StagingOf` is not
`Production.ID`, or where both name the same domain (the second lock would
wait on the first forever).

**Locks.** The two-site handlers take production's lock, then staging's, the
one order every handler uses. `CreateStaging` requires production to exist
(`LockLiveSite`) and takes staging's lock whatever its state: the staging
site is being made. `SyncStagingDatabase` requires both to exist. Inside the
locks only unlocked inner functions run (provisioning, teardown, render):
the lock is not reentrant, and a rollback that took staging's lock again
used to wait on itself forever.

**Selective push of files** is not here: it is a deploy of production with
`SourceSite` and `Overlay` (ops/release SPEC §3.5), which the API's safe push
runs. **Recreate** is the API's: `DeleteSite` of the old copy, a new record,
`CreateStaging`. **Delete** is `DeleteSite`; the staging password file
(`AuthFile`) is part of the site and goes with it (§5).

## 2. Create

1. **Refusals, before anything is made**: the pair check (§1); a WordPress
   production whose database is external (the copy would have no database of
   its own to point at) is `invalid`; so is a staging database name or user
   that is not a name the staging site may take (`internal/site.ClaimableBy`,
   Fase 9b: `^[a-z][a-z0-9_]{2,47}$`, never reserved, never another site's
   `site_<n>`; the API names the copy's `site_<staging id>`): both are wp-cli
   arguments (`config set DB_NAME`, `DB_USER`, step 8), and a value starting
   with `-` would be read as an option (R-10.5), which the shape excludes.
   Production's primary must be recorded as production's (ops/db SPEC §2).
   The check is repeated just before step 8. **Only the primary database is
   staged**: the clone copies production's primary into the copy's, a push
   (§6) moves tables of the copy's primary into production's (both checked
   against the record, after the pair's locks); a site's other databases are
   neither cloned nor pushed (internal/api SPEC §3.7). *Backlog (after the
   release, not in the rewrite plan):* an option to clone all of the site's
   databases into the copy (each under a name of the copy's own, rewritten
   where the application names them) and to push tables of any of them.
   The push's `blogPublic`/`setBlogPublic` (§6) were reviewed with it: the
   database is `db.ValidName`, the table `^[A-Za-z0-9_]{1,64}$`, both
   backquoted, and the values are placeholders; nothing to change.
2. **Skeleton**: the staging site is provisioned like a new site, without
   an application (a WordPress production is provisioned as a plain PHP
   site: the clone replaces the bootstrap). Provisioning refuses to start
   over anything that exists (user, root, database) and removes what it
   made if it fails.
3. **Rollback**: from here on, any failure tears the staging site down
   completely (vhost, pool, database, tree, user), as the caller of an
   unlocked teardown that already holds its lock.
4. **Files**: staging's `releases/initial` is replaced by a copy of
   production's live release, and staging's `shared/uploads` by a copy of
   production's, both with `sitefs.CopyTree` read through production's root
   descriptor: nothing is followed through a link, a link at the top of
   either source fails the clone, hard links become independent files, and
   every created entry belongs to the staging user.
5. **Layout links**: in the copy, `wp-config.php`, `wp-content/uploads` and
   `wp-content/object-cache.php`, where they are links, are made the
   relative links of the layout (ops/release SPEC §1) whatever production's
   pointed at. A relative link already resolves into staging's own
   `shared/`; an absolute one (a site built before R-2.3) would point at
   production's, and the edits below would then reach production's files.
6. **Configuration**: production's `shared/wp-config.php`, read without
   following a link at its name and required to be a regular file, becomes
   staging's (mode 0600, staging's user). A missing one is skipped; any
   other failure to read it fails the clone. When the copy links the object
   cache drop-in, staging gets its own `shared/object-cache.php`.
7. **Database** (production's database enabled and local): production is
   dumped into a root-only file under the agent's work directory, imported
   into staging's database through the sandboxed client as an account
   scoped to that database, and the file removed.
8. **WordPress**, with wp-cli as the staging user on staging's live
   release:
   - `DB_NAME`, `DB_USER` set to staging's; `DB_PASSWORD` set from stdin
     (bare `--prompt`, which is how wp-cli reads the positional value of
     `config set`; a secret never reaches argv);
   - `WP_ENVIRONMENT_TYPE` = `staging`, `DISALLOW_INDEXING` = `true`;
   - `CLOUDGROUND_SITE_TOKEN` set from stdin to staging's own token or,
     when the API gave none and the copied config has one (`config has`),
     deleted, a failed delete failing the clone: staging never holds
     production's (the old clone ignored a failed delete);
   - `config shuffle-salts`: a staging compromise cannot forge production
     login cookies;
   - the URL rewrite (§4) from production's domain to staging's, over all
     tables, `--precise`;
   - `blog_public` = `0`: WordPress then sends `noindex`.
9. **Finish**: the skeleton's ownership is re-asserted (`LockSkeleton`),
   staging's configuration is rendered with its own record — the password
   file and the internal key that lets the panel's own requests past it
   come from `Config.StagingAuthHash` (the vhost is root-only, 0600) — and
   the provisioned site is verified (user, live release, vhost, pool,
   database). The rendered files are returned for the API to record.

## 3. Diff

Production's and staging's live releases are walked through their root
descriptors (`sitefs.Walk`, nothing followed). Only regular files count;
`wp-content/uploads` and every `.git` directory are skipped. A file only on
staging is **added**, only on production **removed**; one on both is
**modified** when the sizes differ, or when the modification times differ
and the contents do not (SHA-256, both opened `O_NOFOLLOW` through their
roots; a file over 8 MiB with a different time is reported modified without
hashing). Each list is sorted and holds at most 1000 paths; a release of
50 000 files or more is not walked further. Either limit sets `Truncated`.

## 4. URL rewrite

`wp search-replace` of the site's address as `//<from>` and, as a second
pass, in its JSON-escaped form `\/\/<from>` (block attributes and other JSON
in post content store URLs that way, and a single pass left them pointing
at the other site). Both schemes are covered by the `//` form.

## 5. Tables and the push policy

`StagingTables` lists staging's database tables (base tables and views,
from `information_schema`, as root). A table is **protected** — never
pushed over production — when its name, lowercased, is:

- a WordPress identity table: ends in `_users` or `_usermeta`;
- on a **store**, live store data: ends in `_posts`, `_postmeta`,
  `_comments`, `_commentmeta` (orders, order notes and reviews live there
  without HPOS), or contains `_wc_order`, `_woocommerce_order`,
  `_woocommerce_sessions`, `_actionscheduler_`, `_wc_customer_lookup`,
  `_woocommerce_payment_token`, `_woocommerce_downloadable_product_permissions`,
  `_wc_download_log`, `_wc_reserved_stock` or `_woocommerce_api_keys`.

Production is a store when it is a WooCommerce site, has the commerce
profile (which installs WooCommerce), or its database has a WooCommerce
table (a name containing `_woocommerce_` or `_wc_`: WooCommerce installed
by hand on a WordPress site). The old policy looked at the site type only,
and let a commerce-profile or hand-installed store push its orders,
customers and saved payment tokens away.

The policy is the agent's; the API asks for it with `StagingTables` and does
not keep a copy.

## 6. Push (SyncStagingDatabase)

1. **Refusals, before any database call**: the pair check; 1 to 50 tables,
   each `^[A-Za-z0-9_]{1,64}$`, no duplicates; tables protected by the
   static policy (site type and profile); invalid database names.
2. **Locks**: production's, then staging's, both live.
3. **Checks**: every table exists in staging's database (`not_found`
   otherwise); none is protected by the full policy (§5, which reads
   production's table list).
4. **Dump** the selected staging tables into a root-only file.
5. **Replace** (ops/db): production's database is saved whole, the dump is
   imported over it (the other tables stay); a failure here or in any later
   step puts production back exactly as it was.
6. **WordPress**: the URL rewrite (§4) from staging's domain to
   production's, limited to the pushed tables, `--skip-columns=guid`, as
   production's user. Then, for each pushed `*_options` table, production's
   own `blog_public` value, read before the import, is written back:
   staging's is `0`, and pushing settings used to de-index production.
7. **Commit**, then **refresh** production (ops/wp `Refresh`): its object
   cache is flushed through its own PHP workers and its PHP reloaded, so the
   pushed rows are what the site serves. Without it, APCu kept serving the
   options and terms from before the push. A failed refresh is logged; the
   push stands.

The API takes and restore-tests the safety snapshot before calling, under
its backup slot (R-0.10), and purges the page cache afterwards.

## 7. Verification

Unit (`staging_test.go`, Linux): the pair checks; the clone's links
relative and resolving inside staging, production's config unchanged; the
policy for each kind of store; a push rejected before any database call;
a push's URL rewrite in both forms, `blog_public` written back, the refresh
called; a failed rewrite rolls production back; the diff's three lists.
Agent (`internal/agent`): rollback without deadlock; a planted link at
production's uploads fails the clone and leaves nothing; lock ordering on a
deleted site; the password file removed with the site.
e2e (`features_test.go`): the R-2.3 layout checks, password protection and
the internal key, diff, safe push, recreate, and the R-3.3 checks in
PARITY.md.
