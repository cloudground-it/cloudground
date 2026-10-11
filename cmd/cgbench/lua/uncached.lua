-- uncached: every request must miss the page cache (bench/SPEC.md §2).
--
-- No query string is ever sent twice, so each response is rendered by PHP and
-- the database. Without this the scenario would quietly measure the cache.
-- wrk gives every thread its own Lua state, so a plain counter would repeat
-- across threads: setup() hands each thread a number, and the thread puts it
-- in front of its own counter.

local threads = 0

function setup(thread)
  threads = threads + 1
  thread:set("tid", threads)
end

local n = 0

function init(args)
  math.randomseed(os.time() + (tid or 0) * 7919)
end

function request()
  n = n + 1
  local q = string.format("?bench=%d-%d-%d", tid or 0, n, math.random(1, 1000000000))
  return wrk.format(nil, wrk.path .. q)
end
