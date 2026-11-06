"""Two writer THREADS inside one database process, killed at a random moment.

Covers scenarios D1 and D2 from the problem statement:
  D1: two writers, different keys  -> every ACKed put from either writer must be present
  D2: two writers, the SAME key    -> key must hold a value one writer really wrote (no mix)

Usage: python concurrent_test.py [--iterations 30] [--seed 1]

How it works: this file starts itself as a child ('--child'), the child runs 2 writer
threads and prints "ACK <writer> <n>" after each put is durable. The parent records those
ACKs (its own oracle), kills the child with a hard kill, reopens the database and checks.
"""
import subprocess, sys, os, random, time, threading, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from kvdb import DB


def child(path):
    db = DB(path)
    out_lock = threading.Lock()

    def writer(wid):
        i = 0
        while True:
            i += 1
            v = f"val-{wid}-{i}".encode()
            db.put(f"w{wid}-{i}".encode(), v)   # D1: this writer's own key
            db.put(b"shared", v)                # D2: both writers fight over this key
            with out_lock:
                print(f"ACK {wid} {i}", flush=True)   # only after BOTH puts are durable

    for wid in (0, 1):
        threading.Thread(target=writer, args=(wid,), daemon=True).start()
    while True:
        time.sleep(1)


def one_round(rnd, path):
    acked = {0: 0, 1: 0}   # writer -> highest n that got an ACK
    p = subprocess.Popen([sys.executable, os.path.abspath(__file__), "--child", path],
                         stdout=subprocess.PIPE, text=True)

    def reader():
        for line in p.stdout:
            if line.startswith("ACK"):
                _, w, n = line.split()
                acked[int(w)] = max(acked[int(w)], int(n))

    t = threading.Thread(target=reader)
    t.start()
    time.sleep(rnd.uniform(0.2, 0.7))   # random kill moment
    p.kill()
    p.wait()
    t.join()

    db = DB(path)
    # D1: every acked key present with the right value
    for w in (0, 1):
        for n in range(1, acked[w] + 1):
            got = db.index.get(f"w{w}-{n}".encode())
            if got != f"val-{w}-{n}".encode():
                return False, f"D1 FAIL: acked w{w}-{n} found {got!r}"
    # no wrong values anywhere
    for k, v in db.index.items():
        k = k.decode()
        if k.startswith("w"):
            w, n = k[1:].split("-")
            if v.decode() != f"val-{w}-{n}":
                return False, f"D1 FAIL: key {k} holds wrong value {v!r}"
    # D2: shared must be a value that was really sent (n at most acked+1 per writer)
    shared = db.index.get(b"shared")
    if shared is None:
        if acked[0] + acked[1] > 0:
            return False, "D2 FAIL: shared missing though puts were ACKed"
    else:
        _, w, n = shared.decode().split("-")
        if not (shared.decode().startswith("val-") and int(n) <= acked[int(w)] + 1):
            return False, f"D2 FAIL: shared holds {shared!r}, never written"
    db.close()
    return True, f"ok (writer0 acked={acked[0]}, writer1 acked={acked[1]})"


def main():
    a = sys.argv[1:]
    if "--child" in a:
        return child(a[a.index("--child") + 1])
    iters = int(a[a.index("--iterations") + 1]) if "--iterations" in a else 30
    seed = int(a[a.index("--seed") + 1]) if "--seed" in a else 1
    rnd = random.Random(seed)
    d = tempfile.mkdtemp()
    for it in range(iters):
        ok, msg = one_round(rnd, os.path.join(d, f"t{it}.db"))
        print(f"iter {it}: {msg}")
        if not ok:
            print("FAIL, stopping.", f"(seed={seed}, iteration={it})")
            sys.exit(1)
    print(f"PASS {iters}/{iters}")


if __name__ == "__main__":
    main()
