<?php
// The little of WordPress the drop-in needs, and an options "database" in a
// JSON file that both processes read, standing in for MariaDB.
define('ABSPATH', __DIR__ . '/');
define('DB_NAME', 'bench');
define('AUTH_KEY', 'bench-key');
$table_prefix = 'wp_';
function wp_suspend_cache_addition() { return false; }
require __DIR__ . '/wp-content/object-cache.php';
wp_cache_init();

function bench_db(): string { return dirname(realpath(__DIR__ . '/wp-content/object-cache.php')) . '/db.json'; }

// get_option('blogname') as WordPress does it: alloptions from the cache,
// else from the database, then cached.
function bench_get(): string {
    $all = wp_cache_get('alloptions', 'options');
    if ($all === false) {
        $all = json_decode((string) @file_get_contents(bench_db()), true) ?: [];
        wp_cache_add('alloptions', $all, 'options');
    }
    return (string) ($all['blogname'] ?? '');
}

// update_option(): the database, then this process's cache.
function bench_set(string $v): void {
    $all = json_decode((string) @file_get_contents(bench_db()), true) ?: [];
    $all['blogname'] = $v;
    file_put_contents(bench_db(), json_encode($all));
    wp_cache_set('alloptions', $all, 'options');
}
