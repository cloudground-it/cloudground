-- browsers: requests shaped like a mix of real visitors (bench/SPEC.md §2).
--
-- Each request takes the next Accept-Encoding value and the next link suffix
-- from the two lists below. Half the suffixes carry a campaign or click tag.
-- A cache that keys on the raw header splits into four entries, and a cache
-- that any query string bypasses sends every tagged visit to PHP. Both show up
-- here as lost throughput, which the fixed-header scenarios cannot see.

local accept = {
  "gzip, deflate, br, zstd", -- current Chromium-based and Gecko browsers
  "gzip, deflate, br",       -- WebKit
  "gzip",                    -- crawlers and the cache warmer
  "br, gzip",
}

local suffix = {
  "",
  "?utm_source=newsletter&utm_medium=email&utm_campaign=autumn",
  "",
  "?fbclid=IwAR0abc123",
  "",
  "?gclid=Cj0KCQjw",
}

local seq = 0

function request()
  seq = seq + 1
  local enc = accept[seq % #accept + 1]
  local tag = suffix[seq % #suffix + 1]
  return wrk.format(nil, wrk.path .. tag, { ["Accept-Encoding"] = enc })
end
