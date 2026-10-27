# `internal/cfips` — Cloudflare's proxy ranges (specification)

The addresses Cloudflare's proxy connects from (#22): the only ones nginx
takes a visitor's address from (`CF-Connecting-IP`, internal/engine SPEC
§4.11), the ones the block list never blocks (internal/blocklist SPEC §1),
and the ones the DNS check calls Cloudflare (internal/api SPEC §3.14). Pure:
no network, no files.

## 1. The shipped list

`Shipped()` is the list published at https://www.cloudflare.com/ips/ when
this release was made (15 IPv4 and 7 IPv6 networks). It is used until the
panel has fetched one (internal/api SPEC §3.20), and whenever the stored
one is missing or no longer passes §2.

## 2. Checking a list (`Parse`)

`Parse(v4, v6 []string) (Ranges, error)` takes the two lists as published
and refuses (the message names the entry):

| Rule | Message |
| --- | --- |
| an entry that is not a CIDR (an address alone included) | `"<s>" is not a network in CIDR form` |
| an IPv4 entry in the IPv6 list, or the other way round | `<p> is in the wrong list` |
| not masked (`104.16.0.1/13`) | `<p> is not a network address` |
| not a global unicast network (loopback, private, link-local, multicast, the unspecified address) | `<p> is not a public network` |
| wider than /8 (IPv4) or /20 (IPv6) | `<p> is too wide to be one of Cloudflare's networks` |
| no IPv4 entry, more than 100 in a list | `a list of Cloudflare's ranges has 1 to 100 networks of each family` |

The result is sorted, without repeats. A fetched list is used only if it
passes: a feed that sent `0.0.0.0/0` or `127.0.0.0/8` would let anyone, or
the server itself, choose the address nginx sees.

`Contains(a)` reports whether an address (unmapped) is in the ranges;
`Prefixes()` is them all, IPv4 first.

## 3. Tests

`cfips_test.go`: the shipped list passes `Parse`; each refusal of §2;
sorting and repeats; `Contains` on an address in and out of the ranges,
IPv4-mapped included.
