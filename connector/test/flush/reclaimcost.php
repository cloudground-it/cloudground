<?php
// The reclaim itself, timed: delete 50000 entries of an old generation.
for ($i = 0; $i < 50000; $i++) { apcu_store("cg:feedfeedfeed:aaaaaaaaaaaaaaaa:0:posts:$i", str_repeat('x', 200)); }
apcu_store('cg:feedfeedfeed:bbbbbbbbbbbbbbbb:0:posts:keep', 1);
$t = hrtime(true);
apcu_delete(new APCUIterator('/^cg:feedfeedfeed:(?!bbbbbbbbbbbbbbbb:|generation$|reclaiming$)/', APC_ITER_KEY, 200));
printf("   reclaiming 50000 orphans itself:         %.1f ms (after the response); live entry kept: %s\n",
    (hrtime(true) - $t) / 1e6, apcu_exists('cg:feedfeedfeed:bbbbbbbbbbbbbbbb:0:posts:keep') ? 'yes' : 'NO');
apcu_delete(new APCUIterator('/^cg:feedfeedfeed:/'));
