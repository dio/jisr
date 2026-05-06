#!/usr/bin/env python3
"""
jisr goroutine leak checker.

Usage: python3 jisr-leak-check.py <admin-port>

What it checks:
  - goroutine count is stable after load (no per-request accumulation)
  - no jisr handler goroutines stuck in chan receive / semacquire after drain

False-positive avoidance:
  - baseline is taken AFTER a warm-up request so Envoy's cgo thread pool
    is fully initialized (those show up as [syscall, locked to thread])
  - locked-to-thread goroutines are excluded from the stable-count check:
    they are Go runtime proxies for Envoy's C worker threads, not Go goroutines
    we spawned, and they don't grow per request
  - server.Group stop-channel waiter is expected in [chan receive] — excluded
"""
import urllib.request
import threading
import time
import re
import sys

if len(sys.argv) < 2:
    print("usage: jisr-leak-check.py <admin-port> [envoy-port]")
    sys.exit(1)

ADMIN = f"http://127.0.0.1:{sys.argv[1]}"
ENVOY = f"http://localhost:{sys.argv[2]}" if len(sys.argv) > 2 else "http://localhost:19000"

PASS = "\033[32mPASS\033[0m"
FAIL = "\033[31mFAIL\033[0m"
WARN = "\033[33mWARN\033[0m"


def fetch(url, timeout=5):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.read().decode()


def goroutine_dump():
    return fetch(f"{ADMIN}/debug/pprof/goroutine?debug=2")


def parse_blocks(dump):
    """Split dump into per-goroutine blocks."""
    return [b.strip() for b in dump.split("\n\n") if b.strip().startswith("goroutine")]


def count_user_goroutines(dump):
    """
    Count goroutines excluding cgo-locked threads.

    [syscall, locked to thread] goroutines are Go runtime entries for Envoy's
    C worker threads — they're created on first CGO call and stay for the
    process lifetime. They don't grow per request and are not leaks.
    """
    locked = 0
    total = 0
    m = re.search(r"goroutine profile: total (\d+)", fetch(f"{ADMIN}/debug/pprof/goroutine?debug=1"))
    if not m:
        return -1, -1
    total = int(m.group(1))
    for block in parse_blocks(dump):
        first = block.split("\n")[0]
        if "syscall, locked to thread" in first:
            locked += 1
    return total, total - locked


def send_request():
    try:
        with urllib.request.urlopen(f"{ENVOY}/", timeout=5) as r:
            r.read()
    except Exception as e:
        print(f"  request error: {e}")


ok = True

# ── Phase 1: warm-up (ensures Envoy's cgo thread pool is initialized) ────────
print("warming up (10 sequential requests)…")
for _ in range(10):
    send_request()
time.sleep(0.3)

dump = goroutine_dump()
total_baseline, user_baseline = count_user_goroutines(dump)
locked_baseline = total_baseline - user_baseline
print(f"[baseline]  total={total_baseline}  locked(cgo)={locked_baseline}  user={user_baseline}")

# ── Phase 2: burst 100 concurrent ────────────────────────────────────────────
print("burst: 100 concurrent requests…")
threads = [threading.Thread(target=send_request) for _ in range(100)]
for t in threads: t.start()
for t in threads: t.join()

dump_peak = goroutine_dump()
total_peak, user_peak = count_user_goroutines(dump_peak)
print(f"[peak]      total={total_peak}  user={user_peak}")

# ── Phase 3: drain ────────────────────────────────────────────────────────────
print("draining (1s)…")
time.sleep(1.0)

dump_drain = goroutine_dump()
total_drain, user_drain = count_user_goroutines(dump_drain)
print(f"[drain]     total={total_drain}  user={user_drain}")

# ── Phase 4: second burst + drain ────────────────────────────────────────────
print("burst: 100 more concurrent requests…")
threads2 = [threading.Thread(target=send_request) for _ in range(100)]
for t in threads2: t.start()
for t in threads2: t.join()
time.sleep(1.0)

dump_final = goroutine_dump()
total_final, user_final = count_user_goroutines(dump_final)
print(f"[final]     total={total_final}  user={user_final}")

# ── Analysis 1: goroutine growth ──────────────────────────────────────────────
print()
delta = user_final - user_baseline
# Allow +2 slack for http keep-alive connections held open by test threads.
if delta <= 2:
    print(f"{PASS}  no goroutine growth (user delta={delta:+d})")
else:
    print(f"{FAIL}  user goroutines grew by {delta} after two bursts — likely leak")
    ok = False

# ── Analysis 2: stuck jisr goroutines ────────────────────────────────────────
# Expected permanent goroutines (not leaks):
#   - server.Group.Start.func2: stop-channel waiter — sits in [chan receive] forever
#   - server.(*Group).AddListener.func1: net.Serve accept loop — sits in [IO wait]
# We flag only jisr goroutines that are NOT one of these known-good patterns.
KNOWN_GOOD = [
    "server.(*Group).Start.func2",           # stop-channel waiter
    "server.(*Group).AddListener",           # accept loop
]

stuck = []
for block in parse_blocks(dump_final):
    first = block.split("\n")[0]
    if "jisr" not in block:
        continue
    # Only flag chan receive / semacquire on unknown goroutines.
    if "chan receive" not in first and "semacquire" not in block:
        continue
    if any(kg in block for kg in KNOWN_GOOD):
        continue
    stuck.append(block)

if stuck:
    print(f"{FAIL}  {len(stuck)} unexpected stuck jisr goroutine(s):")
    for b in stuck:
        print("  " + b[:400].replace("\n", "\n  "))
    ok = False
else:
    print(f"{PASS}  no unexpected stuck jisr goroutines")

# ── Summary ───────────────────────────────────────────────────────────────────
print()
print(f"summary  baseline={user_baseline} peak={user_peak} drain={user_drain} final={user_final}  locked(cgo)={locked_baseline}")
print()
if ok:
    print(f"{PASS}  jisr is leak-free")
else:
    print(f"{FAIL}  see above")
    sys.exit(1)
