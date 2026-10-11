<?php
// What a flush endpoint in the connector does: wp_cache_flush() inside one
// of the site's own FPM workers.
require __DIR__ . '/boot.php';
echo json_encode(['flushed' => wp_cache_flush()]);
