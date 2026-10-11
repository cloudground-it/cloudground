## The change

<!-- What it does and why: the failure it fixes, or the need it meets. Link the issue. -->

## How it was verified

<!--
Needed for every pull request. A build that compiles proves little here: most bugs in this
project's history read as correct and failed on a real server. Say what you ran, for example:
  - make docker-test: green
  - cg-e2e on a throwaway Ubuntu 24.04 server, sections <names>: green; the new check fails on
    the previous build
  - the response headers, the listening socket or the system calls, as seen on that server
-->

## Performance

<!--
Only for caching, request handling or tuning defaults: the numbers before and after, measured
A/B on one server (bench/README.md). Remove this section otherwise.
-->

## Checklist

- [ ] `make docker-test` is green
- [ ] Tried on a real server, not only locally
- [ ] A new check asserts on what the product does, not on what the test set up
- [ ] `SPEC.md` and `docs/` follow the change in behaviour
- [ ] `ui/dist` rebuilt and committed after a change in `ui/app` or `ui/i18n`
