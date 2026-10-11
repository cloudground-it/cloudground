#!/usr/bin/env bash
# Measures the two ways a change made outside PHP-FPM can reach the site's
# APCu object cache (connector/SPEC.md, part 3.4): a new generation written
# by wp-cli, and a flush run inside an FPM worker. Correctness first (the
# control must come back stale), then cost.
#
#   connector/test/flush/run.sh            php:8.4-fpm, APCu from PECL
#   IMAGE=php:8.5-fpm connector/test/flush/run.sh
#
# Needs docker and network access for the image and PECL.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
php=$(cd "$here/../../php" && pwd)
docker run --rm -v "$here":/bench:ro -v "$php":/dropin:ro "${IMAGE:-php:8.4-fpm}" bash /bench/inside.sh
