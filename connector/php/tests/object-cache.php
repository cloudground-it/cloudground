<?php
/**
 * The object-cache drop-in outside WordPress (connector/SPEC.md, part 3).
 * Run it in both states of apc.enable_cli, the two a site sees:
 *
 *   php -d apc.enable_cli=0 connector/php/tests/object-cache.php   wp-cli
 *   php -d apc.enable_cli=1 connector/php/tests/object-cache.php   PHP-FPM (needs the APCu extension)
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

require __DIR__ . '/check.php';

// A scratch copy of the drop-in, so its generation file lands there.
$dir = sys_get_temp_dir() . '/cg-oc-' . getmypid() . '-' . bin2hex(random_bytes(3));
mkdir($dir, 0755, true);
copy(dirname(__DIR__) . '/object-cache.php', $dir . '/object-cache.php');
define('ABSPATH', $dir . '/');
define('DB_NAME', 'test');
define('AUTH_KEY', 'test-key');
$table_prefix = 'wp_';
function wp_suspend_cache_addition() { return false; }
function is_multisite() { return true; }
function get_current_blog_id() { return 1; }
function _deprecated_function() {}
require $dir . '/object-cache.php';

$shared = function_exists('apcu_enabled') && apcu_enabled() && filter_var(ini_get('apc.enable_cli'), FILTER_VALIDATE_BOOLEAN);
printf("shared APCu segment in this process: %s\n", $shared ? 'yes' : 'no');
$file = $dir . '/.cloudground-cache-epoch';

/** A new request: the generation is read again, the request cache is new. */
function new_request(): void
{
    (new ReflectionProperty(CloudGround_Generation::class, 'current'))->setValue(null, null);
    wp_cache_init();
}

section('activation');
check('CLOUDGROUND_OC_APCU says whether the segment is shared', CLOUDGROUND_OC_APCU, $shared);

section('contract');
wp_cache_init();
check('set', wp_cache_set('k', 'v'), true);
check('get', wp_cache_get('k'), 'v');
wp_cache_get('missing', '', false, $found);
check('a miss says found=false', $found, false);
check('false is a value', [wp_cache_set('f', false), wp_cache_get('f', '', false, $found), $found], [true, false, true]);
check('add on a new key (a lock)', wp_cache_add('lock', 1), true);
check('add on a held key', wp_cache_add('lock', 1), false);
check('replace a missing key', wp_cache_replace('nope', 1), false);
check('replace', [wp_cache_replace('k', 'w'), wp_cache_get('k')], [true, 'w']);
check('delete', [wp_cache_delete('k'), wp_cache_get('k')], [true, false]);
check('incr of a missing key', wp_cache_incr('n'), false);
wp_cache_set('n', 5);
check('incr', wp_cache_incr('n', 3), 8);
check('decr', wp_cache_decr('n', 2), 6);
check('decr stops at zero', wp_cache_decr('n', 100), 0);
$o = new stdClass();
$o->a = 1;
wp_cache_set('obj', $o);
$o->a = 2;
check('objects are stored by value', wp_cache_get('obj')->a, 1);
$g = wp_cache_get('obj');
$g->a = 3;
check('objects come out by value', wp_cache_get('obj')->a, 1);
check('set_multiple', wp_cache_set_multiple(['a' => 1, 'b' => 2], 'grp'), ['a' => true, 'b' => true]);
check('get_multiple', wp_cache_get_multiple(['a', 'b', 'c'], 'grp'), ['a' => 1, 'b' => 2, 'c' => false]);
check('add_multiple', wp_cache_add_multiple(['a' => 9, 'z' => 9], 'grp'), ['a' => false, 'z' => true]);
check('delete_multiple', wp_cache_delete_multiple(['a', 'q'], 'grp'), ['a' => true, 'q' => false]);
wp_cache_add_non_persistent_groups(['counts']);
wp_cache_set('c', 1, 'counts');
wp_cache_add_global_groups(['users']);
wp_cache_set('u', 'global', 'users');
wp_cache_set('local', 'blog1', 'posts');
wp_cache_switch_to_blog(2);
check('a global group is shared by the blogs of a network', wp_cache_get('u', 'users'), 'global');
check('other groups are per blog', wp_cache_get('local', 'posts'), false);
wp_cache_switch_to_blog(1);
check('supports', [wp_cache_supports('get_multiple'), wp_cache_supports('flush_group'), wp_cache_supports('nope')], [true, true, false]);

section('across requests');
wp_cache_set('kept', 'yes');
new_request();
wp_cache_add_non_persistent_groups(['counts']);
check($shared ? 'a value outlives the request in APCu' : 'without APCu nothing outlives the request', wp_cache_get('kept'), $shared ? 'yes' : false);
check('a non-persistent group never does', wp_cache_get('c', 'counts'), false);

section('generation');
check('the generation file is created next to the drop-in', is_file($file), true);
$before = trim((string) file_get_contents($file));
check('16 hex characters', preg_match('/\A[0-9a-f]{16}\z/', $before), 1);
check('readable by the web server', decoct(fileperms($file) & 0777), '644');
wp_cache_set('old', 'value');
check('flush', wp_cache_flush(), true);
$after = trim((string) file_get_contents($file));
check('flush writes a new generation', $after !== $before, true);
check('this request sees nothing from before', wp_cache_get('old'), false);
new_request();
check('a new request (another process: wp-cli or FPM) reads the new generation', wp_cache_get('old'), false);
check('…and keeps it', trim((string) file_get_contents($file)), $after);
check('a cache that works after a flush', [wp_cache_set('n2', 1), wp_cache_get('n2')], [true, 1]);
file_put_contents($file, 'not a generation');
new_request();
check('a damaged file is replaced', preg_match('/\A[0-9a-f]{16}\z/', trim((string) file_get_contents($file))), 1);
check('no temporary files left', glob($dir . '/.cloudground-cache-epoch.*'), []);

section('flush_group');
wp_cache_set('p1', 1, 'posts');
wp_cache_set('o1', 1, 'options');
$gen = trim((string) file_get_contents($file));
check('flush_group', wp_cache_flush_group('posts'), true);
check('the group is gone', wp_cache_get('p1', 'posts'), false);
if ($shared) {
    check('in PHP-FPM other groups stay', wp_cache_get('o1', 'options'), 1);
    check('…and the generation stays', trim((string) file_get_contents($file)), $gen);
} else {
    check('in wp-cli it can only reach PHP-FPM by a new generation', trim((string) file_get_contents($file)) !== $gen, true);
}
check('flush_runtime', wp_cache_flush_runtime(), true);

section('wp-cli changes reach PHP-FPM (#219)');
$stale = $dir . '/.cloudground-cache-stale';
@unlink($stale);
new_request();
$idOf = fn (string $key, string $group) => (new ReflectionMethod(WP_Object_Cache::class, 'id'))->invoke($GLOBALS['wp_object_cache'], $key, $group);
if ($shared) {
    $u = $idOf('7', 'users');
    wp_cache_set('7', 'old hash', 'users');
    wp_cache_set('8', 'kept', 'users');
    file_put_contents($stale, $u . "\ncg:000000000000:other-site-key\n");
    new_request();
    check('a key wp-cli changed is dropped at the next request', wp_cache_get('7', 'users'), false);
    check('other keys stay', wp_cache_get('8', 'users'), 'kept');
    check('the list is emptied', filesize($stale), 0);
} else {
    wp_cache_add_non_persistent_groups('counts');
    wp_cache_set('7', 'new hash', 'users');
    wp_cache_delete('9', 'users');
    wp_cache_add('fresh', 1, 'posts');
    wp_cache_set('c', 1, 'counts');
    check('nothing is written before the end of the run', is_file($stale), false);
    check('wp_cache_close', wp_cache_close(), true);
    $lines = file($stale, FILE_IGNORE_NEW_LINES);
    check('set and delete are listed, add and non-persistent groups are not', $lines, [$idOf('7', 'users'), $idOf('9', 'users')]);
    check('a second close adds nothing', [wp_cache_close(), count(file($stale))], [true, 2]);
    new_request();
    wp_cache_set('x', 1);
    wp_cache_flush();
    wp_cache_close();
    check('after a flush in the run nothing more is listed', count(file($stale)), 2);
    new_request();
    $gen = trim((string) file_get_contents($file));
    for ($i = 0; $i <= 2000; $i++) {
        wp_cache_set("many$i", $i);
    }
    wp_cache_close();
    check('too many keys: a new generation instead', trim((string) file_get_contents($file)) !== $gen, true);
    check('…and the list is emptied', filesize($stale), 0);
}
new_request();

section('unwritable generation');
chmod($dir, 0555);
if (@file_put_contents($dir . '/probe', 'x') === false) {
    $kept = trim((string) file_get_contents($file));
    check('flush says whether another process can see it', wp_cache_flush(), $shared);
    check('the file is unchanged, nothing half-written', trim((string) file_get_contents($file)), $kept);
    wp_cache_set('still', 'works');
    check('the cache still works', wp_cache_get('still'), 'works');
} else {
    printf("running as root: the unwritable case is not testable here\n");
    @unlink($dir . '/probe');
}
chmod($dir, 0755);
new_request();

if ($shared) {
    section('reclaim (PHP-FPM)');
    $site = substr(array_values(array_filter(array_keys(iterator_to_array(new APCUIterator('/^cg:/', APC_ITER_KEY))), fn ($k) => str_ends_with($k, ':generation')))[0] ?? '', 0, -strlen('generation'));
    check('the generation last seen is recorded', $site !== '', true);
    for ($i = 0; $i < 100; $i++) {
        wp_cache_set("bulk$i", $i);
    }
    wp_cache_flush();
    wp_cache_set('live', 1);
    // The reclaim runs last at shutdown; check after it.
    register_shutdown_function(function () use ($site, $dir) {
        register_shutdown_function(function () use ($site, $dir) {
            $left = 0;
            $gen = trim((string) file_get_contents($dir . '/.cloudground-cache-epoch'));
            foreach (new APCUIterator('/^' . preg_quote($site, '/') . '/', APC_ITER_KEY) as $k => $_) {
                if (!str_starts_with($k, $site . $gen . ':') && !str_ends_with($k, 'generation')) {
                    $left++;
                }
            }
            check('old generations are deleted after the response', $left, 0);
            check('the live generation stays', apcu_exists($site . $gen . ':1:default:live'), true);
            cleanup($dir);
            done();
        });
    });
    return;
}

function cleanup(string $dir): void
{
    foreach (scandir($dir) ?: [] as $f) {
        if ($f !== '.' && $f !== '..') {
            unlink($dir . '/' . $f);
        }
    }
    @rmdir($dir);
}
cleanup($dir);
done();
