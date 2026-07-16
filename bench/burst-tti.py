"""Single-node BURST time-to-interactive: fire N create->run_code->terminate
concurrently (ComputeSDK-style burst), to quantify how much parallelism one KVM
node absorbs before TTI degrades. Reports per-request TTI percentiles, wall clock
to all-ready, and success rate. Requires a tier that allows N concurrent (the
bench org holds pro)."""
import os, sys, time, threading
import mitos

N = int(sys.argv[1]) if len(sys.argv) > 1 else 16
KEY = os.environ["MITOS_API_KEY"]
results = []
lock = threading.Lock()
start_barrier = threading.Barrier(N)

def one(i):
    start_barrier.wait()  # release all threads simultaneously = true burst
    t0 = time.monotonic()
    row = {"i": i, "tti": None, "err": ""}
    sb = None
    try:
        sb = mitos.create("python", api_key=KEY)
        sb.run_code("print(1)", timeout=45)
        row["tti"] = (time.monotonic() - t0) * 1000
    except Exception as e:
        row["tti"] = (time.monotonic() - t0) * 1000
        row["err"] = f"{type(e).__name__}: {str(e)[:80]}"
    finally:
        if sb is not None:
            try: sb.terminate()
            except Exception: pass
    with lock:
        results.append(row)

wall0 = time.monotonic()
threads = [threading.Thread(target=one, args=(i,)) for i in range(N)]
for t in threads: t.start()
for t in threads: t.join()
wall = (time.monotonic() - wall0) * 1000

ok = [r["tti"] for r in results if not r["err"]]
ok.sort()
def pct(p):
    if not ok: return float("nan")
    k = max(0, min(len(ok)-1, int(round(p/100*len(ok)+0.5))-1))
    return ok[k]
errs = [r for r in results if r["err"]]
print(f"BURST N={N}  concurrent create -> run_code -> terminate")
print(f"  wall clock (all ready): {wall:.0f} ms")
print(f"  TTI P50 {pct(50):.1f}  P90 {pct(90):.1f}  P99 {pct(99):.1f}  min {ok[0] if ok else float('nan'):.1f}  max {ok[-1] if ok else float('nan'):.1f} ms")
print(f"  success: {len(ok)}/{N}")
for e in errs[:6]:
    print(f"  FAIL i={e['i']} after {e['tti']:.0f}ms: {e['err']}")
