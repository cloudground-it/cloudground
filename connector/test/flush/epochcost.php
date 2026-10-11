<?php
// What the generation costs a request: one small file read plus one APCu
// fetch, timed in a loop.
$f = dirname(realpath(__DIR__ . '/wp-content/object-cache.php')) . '/.cloudground-cache-epoch';
$n = 100000;
$t = hrtime(true);
for ($i = 0; $i < $n; $i++) { $x = file_get_contents($f, false, null, 0, 64); }
$read = (hrtime(true) - $t) / $n;
$t = hrtime(true);
for ($i = 0; $i < $n; $i++) { $x = apcu_fetch('cg:x:generation'); }
$fetch = (hrtime(true) - $t) / $n;
printf("per request: generation file read %.2f us, generation-seen fetch %.2f us\n", $read / 1000, $fetch / 1000);
