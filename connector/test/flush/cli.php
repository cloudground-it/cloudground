<?php
// wp-cli's side: "wp option update" and "wp cache flush".
require __DIR__ . '/boot.php';
if (($argv[1] ?? '') === 'set') { bench_set($argv[2]); echo "set\n"; }
if (($argv[1] ?? '') === 'flush') { echo json_encode(['flushed' => wp_cache_flush()]), "\n"; }
