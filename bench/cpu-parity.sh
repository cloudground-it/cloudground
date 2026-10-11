#!/bin/sh
# Is this box still the box the last numbers came from? (bench/SPEC.md §5)
# Prints the fixed loop's time, steal, CPU model and cores; exits 1 when the
# hypervisor is stealing CPU. LOOPS changes the loop length.
set -eu
exec "$(cd -- "$(dirname -- "$0")" && pwd)/cgbench" parity
