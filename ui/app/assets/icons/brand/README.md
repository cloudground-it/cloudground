# Software marks

The marks of the software a site type installs, shown next to its name in the new-site wizard and
wherever the panel names a site's type. They are the path data of [Simple Icons](https://simpleicons.org)
(`simple-icons@13.21.0`), whose icon files are released under
[CC0 1.0](https://github.com/simple-icons/simple-icons/blob/develop/LICENSE.md); only `fill="currentColor"`
was added, so they take the text colour like the rest of the panel's icons.

| File | Simple Icons slug | Mark of | Owner's guidelines |
| --- | --- | --- | --- |
| `wordpress.svg` | `wordpress` | WordPress | [WordPress Foundation trademark policy](https://wordpressfoundation.org/trademark-policy/) |
| `woocommerce.svg` | `woo` | WooCommerce (the "Woo" mark) | [WooCommerce trademark guidelines](https://woocommerce.com/trademark-guidelines/) |
| `php.svg` | `php` | PHP | [PHP logo terms](https://www.php.net/download-logos.php) |
| `laravel.svg` | `laravel` | Laravel | [Laravel trademark guidelines](https://laravel.com/trademark) |

`woocommerce.svg` is the upstream path moved up by 4.857 units (`viewBox` `0 0 24 14.3` instead of
`0 4.857 24 14.3`), so the collection loader does not have to carry a viewBox offset. Nothing else
differs from upstream.

CC0 covers the drawing, not the marks: each one remains its owner's trademark. CloudGround uses them
only to name the software a site runs (nominative use), unmodified, monochrome and at icon size,
never as its own logo or to suggest endorsement. Static sites and reverse proxies have no
third-party mark and use the CloudGround icons `file-code` and `network`.

`nuxt.config.ts` registers this directory as the Iconify collection `brand` (`i-brand-<file name>`),
bundled into the client like `cg/`: nothing is fetched at runtime, as the panel's Content Security
Policy requires.
