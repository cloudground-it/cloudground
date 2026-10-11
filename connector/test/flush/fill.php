<?php
// A warm site: n entries in the current generation.
require __DIR__ . '/boot.php';
parse_str($_SERVER['QUERY_STRING'] ?? '', $q);
$n = (int) ($q['n'] ?? 1000);
for ($i = 0; $i < $n; $i++) { wp_cache_set("k$i", str_repeat('x', 200), 'posts'); }
echo $n;
