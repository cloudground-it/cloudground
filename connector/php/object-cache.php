<?php
/**
 * CloudGround object cache: WordPress's persistent object cache in APCu.
 *
 * Installed by the CloudGround agent as shared/object-cache.php and linked
 * from wp-content/object-cache.php. The contract is connector/SPEC.md, part 3.
 *
 * Copyright (C) 2026 CloudGround contributors
 * SPDX-License-Identifier: GPL-2.0-or-later
 *
 * This program is free software; you can redistribute it and/or modify it
 * under the terms of the GNU General Public License as published by the Free
 * Software Foundation; either version 2 of the License, or (at your option)
 * any later version. It is distributed WITHOUT ANY WARRANTY; see the GNU
 * General Public License for more details.
 */

defined('ABSPATH') || exit;

/*
 * Each site has its own PHP-FPM master, so its APCu segment holds this site
 * and nothing else. wp-cli is a different process: with apc.enable_cli off
 * (CloudGround's setting) the segment is never created there, and even with
 * it on the CLI segment dies with the process. In either case the cache
 * keeps working inside the request, like WordPress's own non-persistent
 * cache, and a flush still reaches the web tier through the generation file
 * (SPEC 3.3).
 */
define('CLOUDGROUND_OC_APCU', (static function (): bool {
    if (!function_exists('apcu_enabled') || !apcu_enabled()) {
        return false;
    }
    // apcu_enabled() reads apc.enabled only; in a CLI process the segment
    // exists only when apc.enable_cli is on too.
    return PHP_SAPI !== 'cli' || filter_var(ini_get('apc.enable_cli'), FILTER_VALIDATE_BOOLEAN);
})());

final class CloudGround_Generation
{
    /** The file every process of this site reads its key generation from. */
    public const FILE = '.cloudground-cache-epoch';

    /** What a site with an unwritable generation file runs on. */
    public const FIXED = '0000000000000000';

    private static ?string $current = null;

    public static function path(): string
    {
        // __DIR__ is where the drop-in really is: PHP resolves the link from
        // wp-content, so this is the site's shared/ directory.
        return __DIR__ . '/' . self::FILE;
    }

    /** The generation this request uses, read once per request. */
    public static function current(): string
    {
        return self::$current ??= self::read() ?? self::next();
    }

    /** What the file says now, or null when it says nothing usable. */
    public static function read(): ?string
    {
        $raw = @file_get_contents(self::path(), false, null, 0, 64);
        $raw = is_string($raw) ? trim($raw) : '';
        return preg_match('/\A[0-9a-f]{16}\z/', $raw) === 1 ? $raw : null;
    }

    /**
     * Start a new generation and return it. Random, never a counter: a lost
     * file restarting a counter would revive keys an earlier generation
     * wrote. FIXED means the file could not be written.
     */
    public static function next(): string
    {
        $value = bin2hex(random_bytes(8));
        $file = self::path();
        $tmp = $file . '.' . getmypid() . '.' . bin2hex(random_bytes(4));
        if (@file_put_contents($tmp, $value) === strlen($value) && @chmod($tmp, 0644) && @rename($tmp, $file)) {
            return self::$current = $value;
        }
        @unlink($tmp);
        return self::$current = self::FIXED;
    }
}

class WP_Object_Cache
{
    public int $cache_hits = 0;
    public int $cache_misses = 0;

    /** @var array<string, mixed> values this request has read or written */
    private array $local = [];
    /** @var array<string, true> */
    private array $global_groups = [];
    /** @var array<string, true> */
    private array $local_groups = [];

    private bool $shared;
    private bool $multisite;
    private int $blog = 0;
    private string $site;
    private string $prefix;

    public function __construct()
    {
        $this->shared = CLOUDGROUND_OC_APCU;
        $this->multisite = function_exists('is_multisite') && is_multisite();
        if ($this->multisite && function_exists('get_current_blog_id')) {
            $this->blog = (int) get_current_blog_id();
        }
        $seed = (defined('DB_NAME') ? DB_NAME : '') . '|' . ($GLOBALS['table_prefix'] ?? '') . '|' . (defined('AUTH_KEY') ? AUTH_KEY : '');
        $this->site = 'cg:' . substr(hash('sha256', $seed), 0, 12) . ':';
        $this->prefix = $this->site . CloudGround_Generation::current() . ':';
        if ($this->shared) {
            $this->reclaimIfMoved();
        }
    }

    // ---------------------------------------------------------------- keys

    private function id($key, string $group): string
    {
        $group = $group === '' ? 'default' : $group;
        $blog = isset($this->global_groups[$group]) ? 0 : $this->blog;
        return $this->prefix . $blog . ':' . $group . ':' . $key;
    }

    private function inApcu(string $group): bool
    {
        return $this->shared && !isset($this->local_groups[$group === '' ? 'default' : $group]);
    }

    private static function copy(mixed $value): mixed
    {
        return is_object($value) ? clone $value : $value;
    }

    // ------------------------------------------------------------ reading

    public function get($key, string $group = 'default', bool $force = false, &$found = null): mixed
    {
        $id = $this->id($key, $group);
        if (!$force && array_key_exists($id, $this->local)) {
            $found = true;
            $this->cache_hits++;
            return self::copy($this->local[$id]);
        }
        if ($this->inApcu($group)) {
            $value = apcu_fetch($id, $ok);
            if ($ok) {
                $this->local[$id] = $value;
                $found = true;
                $this->cache_hits++;
                return self::copy($value);
            }
        }
        $found = false;
        $this->cache_misses++;
        return false;
    }

    /** @return array<int|string, mixed> */
    public function get_multiple(array $keys, string $group = 'default', bool $force = false): array
    {
        $out = [];
        $ask = [];
        foreach ($keys as $key) {
            $id = $this->id($key, $group);
            if (!$force && array_key_exists($id, $this->local)) {
                $out[$key] = self::copy($this->local[$id]);
                $this->cache_hits++;
                continue;
            }
            $out[$key] = false;
            $ask[$id] = $key;
        }
        if ($ask !== [] && $this->inApcu($group)) {
            $got = apcu_fetch(array_keys($ask));
            foreach (is_array($got) ? $got : [] as $id => $value) {
                $this->local[$id] = $value;
                $out[$ask[$id]] = self::copy($value);
                unset($ask[$id]);
                $this->cache_hits++;
            }
        }
        $this->cache_misses += count($ask);
        return $out;
    }

    // ------------------------------------------------------------ writing

    public function add($key, mixed $data, string $group = 'default', int $expire = 0): bool
    {
        if (function_exists('wp_suspend_cache_addition') && wp_suspend_cache_addition()) {
            return false;
        }
        $id = $this->id($key, $group);
        if (array_key_exists($id, $this->local)) {
            return false;
        }
        // apcu_add is atomic: WordPress takes locks with add (doing_cron,
        // upgrader locks), and two workers must not both get one.
        if ($this->inApcu($group) && !apcu_add($id, self::copy($data), max(0, $expire))) {
            return false;
        }
        $this->local[$id] = self::copy($data);
        return true;
    }

    public function set($key, mixed $data, string $group = 'default', int $expire = 0): bool
    {
        $id = $this->id($key, $group);
        $this->local[$id] = self::copy($data);
        return !$this->inApcu($group) || apcu_store($id, $this->local[$id], max(0, $expire));
    }

    public function replace($key, mixed $data, string $group = 'default', int $expire = 0): bool
    {
        $this->get($key, $group, false, $found);
        return $found && $this->set($key, $data, $group, $expire);
    }

    /** @return array<int|string, bool> */
    public function add_multiple(array $data, string $group = 'default', int $expire = 0): array
    {
        $out = [];
        foreach ($data as $key => $value) {
            $out[$key] = $this->add($key, $value, $group, $expire);
        }
        return $out;
    }

    /** @return array<int|string, bool> */
    public function set_multiple(array $data, string $group = 'default', int $expire = 0): array
    {
        $store = [];
        $out = [];
        foreach ($data as $key => $value) {
            $id = $this->id($key, $group);
            $this->local[$id] = self::copy($value);
            $store[$id] = $this->local[$id];
            $out[$key] = true;
        }
        if ($store !== [] && $this->inApcu($group)) {
            $failed = apcu_store($store, null, max(0, $expire));
            foreach (is_array($failed) ? $failed : [] as $id => $_) {
                $key = substr((string) $id, strlen($this->id('', $group)));
                $out[$key] = false;
            }
        }
        return $out;
    }

    public function delete($key, string $group = 'default'): bool
    {
        $id = $this->id($key, $group);
        $had = array_key_exists($id, $this->local);
        unset($this->local[$id]);
        if (!$this->inApcu($group)) {
            return $had;
        }
        return apcu_delete($id) || $had;
    }

    /** @return array<int|string, bool> */
    public function delete_multiple(array $keys, string $group = 'default'): array
    {
        $out = [];
        foreach ($keys as $key) {
            $out[$key] = $this->delete($key, $group);
        }
        return $out;
    }

    public function incr($key, int $offset = 1, string $group = 'default'): int|false
    {
        $id = $this->id($key, $group);
        if (!$this->inApcu($group)) {
            if (!array_key_exists($id, $this->local)) {
                return false;
            }
            $value = max(0, (is_numeric($this->local[$id]) ? (int) $this->local[$id] : 0) + $offset);
            return $this->local[$id] = $value;
        }
        // Compare-and-swap, so concurrent increments are not lost; a value
        // stored as something other than an integer is replaced once.
        for ($try = 0; $try < 8; $try++) {
            $old = apcu_fetch($id, $ok);
            if (!$ok) {
                unset($this->local[$id]);
                return false;
            }
            $value = max(0, (is_numeric($old) ? (int) $old : 0) + $offset);
            if (is_int($old) ? apcu_cas($id, $old, $value) : apcu_store($id, $value)) {
                return $this->local[$id] = $value;
            }
        }
        return false;
    }

    public function decr($key, int $offset = 1, string $group = 'default'): int|false
    {
        return $this->incr($key, -$offset, $group);
    }

    // ------------------------------------------------------------ flushing

    /**
     * Forget everything this site cached, in every process: a new generation
     * changes every key, here and in PHP-FPM. False only when the generation
     * file cannot be written, because then no other process can see it.
     */
    public function flush(): bool
    {
        $this->local = [];
        $next = CloudGround_Generation::next();
        if ($next === CloudGround_Generation::FIXED) {
            // No other process can be told. Inside PHP-FPM the segment can
            // still be emptied directly; anywhere else this flush failed.
            if ($this->shared) {
                $this->deleteMatching('/^' . preg_quote($this->site, '/') . '/');
            }
            return $this->shared;
        }
        $this->prefix = $this->site . $next . ':';
        if ($this->shared) {
            $this->reclaimIfMoved();
        }
        return true;
    }

    public function flush_runtime(): bool
    {
        $this->local = [];
        return true;
    }

    /**
     * Forget one group. Without the shared segment this process cannot
     * reach PHP-FPM's copy, so the only correct thing is a full flush.
     */
    public function flush_group(string $group): bool
    {
        if (!$this->shared) {
            return $this->flush();
        }
        $prefix = $this->id('', $group);
        foreach (array_keys($this->local) as $id) {
            if (str_starts_with((string) $id, $prefix)) {
                unset($this->local[$id]);
            }
        }
        // The blog part of the key differs per site of a network; a group
        // flush covers all of them, as WordPress's own does.
        $g = $group === '' ? 'default' : $group;
        $this->deleteMatching('/^' . preg_quote($this->prefix, '/') . '\d+:' . preg_quote($g, '/') . ':/');
        return true;
    }

    /**
     * A new generation leaves the old keys unreachable but still in memory.
     * The first request of this site that sees the change deletes them,
     * after its response is sent. One apcu_fetch per request decides it.
     */
    private function reclaimIfMoved(): void
    {
        $seen = $this->site . 'generation';
        $now = substr($this->prefix, strlen($this->site), 16);
        if (apcu_fetch($seen) === $now) {
            return;
        }
        apcu_store($seen, $now);
        if (!apcu_add($this->site . 'reclaiming', 1, 60)) {
            return;
        }
        $site = $this->site;
        $reclaim = function () use ($site, $now): void {
            // Last of all, with the response already sent.
            if (function_exists('fastcgi_finish_request')) {
                fastcgi_finish_request();
            }
            // The generation may have moved again since this was scheduled
            // (a flush in this request, or from wp-cli): keep the newest.
            $keep = CloudGround_Generation::read() ?? $now;
            $this->deleteMatching('/^' . preg_quote($site, '/') . '(?!' . $keep . ':|generation$|reclaiming$)/');
            apcu_delete($site . 'reclaiming');
        };
        // Registered from a shutdown function, it runs after every other one,
        // WordPress's own shutdown included.
        register_shutdown_function(static function () use ($reclaim): void {
            register_shutdown_function($reclaim);
        });
    }

    private function deleteMatching(string $regex): void
    {
        if (!class_exists('APCUIterator')) {
            return;
        }
        try {
            apcu_delete(new APCUIterator($regex, APC_ITER_KEY, 200));
        } catch (Throwable) {
            // The segment is gone or unusable: nothing to reclaim.
        }
    }

    // --------------------------------------------------------------- groups

    public function add_global_groups(string|array $groups): void
    {
        foreach ((array) $groups as $g) {
            $this->global_groups[$g] = true;
        }
    }

    public function add_non_persistent_groups(string|array $groups): void
    {
        foreach ((array) $groups as $g) {
            $this->local_groups[$g] = true;
        }
    }

    public function switch_to_blog(int $blog_id): void
    {
        $this->blog = $this->multisite ? $blog_id : 0;
    }

    public function stats(): void
    {
        printf('<p>Object cache (APCu %s): %d hits, %d misses</p>', $this->shared ? 'shared' : 'per request', $this->cache_hits, $this->cache_misses);
    }
}

// ----------------------------------------------------- WordPress's API ----

function wp_cache_init(): void
{
    $GLOBALS['wp_object_cache'] = new WP_Object_Cache();
}

function wp_cache_add($key, $data, $group = '', $expire = 0)
{
    return $GLOBALS['wp_object_cache']->add($key, $data, (string) $group, (int) $expire);
}

function wp_cache_add_multiple(array $data, $group = '', $expire = 0)
{
    return $GLOBALS['wp_object_cache']->add_multiple($data, (string) $group, (int) $expire);
}

function wp_cache_set($key, $data, $group = '', $expire = 0)
{
    return $GLOBALS['wp_object_cache']->set($key, $data, (string) $group, (int) $expire);
}

function wp_cache_set_multiple(array $data, $group = '', $expire = 0)
{
    return $GLOBALS['wp_object_cache']->set_multiple($data, (string) $group, (int) $expire);
}

function wp_cache_replace($key, $data, $group = '', $expire = 0)
{
    return $GLOBALS['wp_object_cache']->replace($key, $data, (string) $group, (int) $expire);
}

function wp_cache_get($key, $group = '', $force = false, &$found = null)
{
    return $GLOBALS['wp_object_cache']->get($key, (string) $group, (bool) $force, $found);
}

function wp_cache_get_multiple($keys, $group = '', $force = false)
{
    return $GLOBALS['wp_object_cache']->get_multiple((array) $keys, (string) $group, (bool) $force);
}

function wp_cache_delete($key, $group = '')
{
    return $GLOBALS['wp_object_cache']->delete($key, (string) $group);
}

function wp_cache_delete_multiple(array $keys, $group = '')
{
    return $GLOBALS['wp_object_cache']->delete_multiple($keys, (string) $group);
}

function wp_cache_incr($key, $offset = 1, $group = '')
{
    return $GLOBALS['wp_object_cache']->incr($key, (int) $offset, (string) $group);
}

function wp_cache_decr($key, $offset = 1, $group = '')
{
    return $GLOBALS['wp_object_cache']->decr($key, (int) $offset, (string) $group);
}

function wp_cache_flush()
{
    return $GLOBALS['wp_object_cache']->flush();
}

function wp_cache_flush_runtime()
{
    return $GLOBALS['wp_object_cache']->flush_runtime();
}

function wp_cache_flush_group($group)
{
    return $GLOBALS['wp_object_cache']->flush_group((string) $group);
}

function wp_cache_supports($feature)
{
    return in_array($feature, ['add_multiple', 'set_multiple', 'get_multiple', 'delete_multiple', 'flush_runtime', 'flush_group'], true);
}

function wp_cache_close()
{
    return true;
}

function wp_cache_add_global_groups($groups)
{
    $GLOBALS['wp_object_cache']->add_global_groups($groups);
}

function wp_cache_add_non_persistent_groups($groups)
{
    $GLOBALS['wp_object_cache']->add_non_persistent_groups($groups);
}

function wp_cache_switch_to_blog($blog_id)
{
    $GLOBALS['wp_object_cache']->switch_to_blog((int) $blog_id);
}

function wp_cache_reset()
{
    _deprecated_function(__FUNCTION__, '3.5.0', 'wp_cache_switch_to_blog()');
    return false;
}
