<?php
// wp-cli's side: "wp option update", "wp cache flush", and a change made
// in the database alone ("wp db query"), which no cache call follows.
require __DIR__ . '/boot.php';
if (($argv[1] ?? '') === 'set') { bench_set($argv[2]); echo "set\n"; }
if (($argv[1] ?? '') === 'db') { $all = json_decode((string) @file_get_contents(bench_db()), true) ?: []; $all['blogname'] = $argv[2]; file_put_contents(bench_db(), json_encode($all)); echo "db\n"; }
if (($argv[1] ?? '') === 'flush') { echo json_encode(['flushed' => wp_cache_flush()]), "\n"; }
