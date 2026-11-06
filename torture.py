"""Mini torture tool: random ops -> kill -9 at random ms -> reopen -> compare with oracle.

Usage: python torture.py [--iterations 100] [--seed 1]

Oracle = ops the tool itself saw ACKed. It never trusts the DB to tell it what was promised.
Rule checked: for each key, the DB value must be the last ACKed value, or the value of a
LATER op that was sent but not ACKed (it may or may not have hit disk). Anything else = bug.
"""
import subprocess, sys, os, random, time, threading, tempfile, itertools

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from kvdb import DB

ENGINE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "kvdb.py")
KEYS = [f"k{i}" for i in range(30)]
counter = itertools.count()


def one_round(rnd, path, state):
    ops = {}          # id -> (key, value or None)
    per_key = {}      # key -> [ids sent, in order]
    last_ack = {}     # key -> highest ACKed id
    p = subprocess.Popen([sys.executable, ENGINE, "serve", path],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)

    def reader():
        for line in p.stdout:
            if line.startswith("ACK"):
                i = int(line.split()[1])
                k = ops[i][0]
                last_ack[k] = max(last_ack.get(k, -1), i)

    def writer():
        try:
            while True:
                i = next(counter)
                k = rnd_w.choice(KEYS)
                if rnd_w.random() < 0.25:
                    ops[i] = (k, None); line = f"{i} del {k}\n"
                else:
                    v = f"v{i}-" + "x" * rnd_w.randint(0, 200)
                    ops[i] = (k, v); line = f"{i} put {k} {v}\n"
                per_key.setdefault(k, []).append(i)
                p.stdin.write(line); p.stdin.flush()
        except (BrokenPipeError, OSError, ValueError):
            pass

    rnd_w = random.Random(rnd.random())
    tr, tw = threading.Thread(target=reader), threading.Thread(target=writer)
    tr.start(); tw.start()
    time.sleep(rnd.uniform(0.15, 0.6))   # random kill moment in ms, NOT "after N ops"
    p.kill()                              # SIGKILL
    p.wait(); tr.join(); tw.join()
    try:
        p.stdin.close()
    except OSError:
        pass

    db = DB(path)
    for k in KEYS:
        got = db.index.get(k.encode())
        got = got.decode() if got is not None else None
        if k in last_ack:
            allowed = [ops[i][1] for i in per_key[k] if i >= last_ack[k]]
        else:
            allowed = [state.get(k)] + [ops[i][1] for i in per_key.get(k, [])]
        if got not in allowed:
            return False, f"key {k}: found {got!r}, allowed {allowed[:5]}, last_ack={last_ack.get(k)}"
    extra = {k.decode() for k in db.index} - set(KEYS)
    if extra:
        return False, f"unknown keys {extra}"
    state.clear()
    state.update({k.decode(): v.decode() for k, v in db.index.items()})
    n_acked = len(last_ack)
    db.close()
    return True, f"ok (acked-keys={n_acked})"


def main():
    a = sys.argv[1:]
    iters = int(a[a.index("--iterations") + 1]) if "--iterations" in a else 100
    seed = int(a[a.index("--seed") + 1]) if "--seed" in a else 1
    rnd = random.Random(seed)
    d = tempfile.mkdtemp()
    path, state, passed = os.path.join(d, "t.db"), {}, 0
    for it in range(iters):
        ok, msg = one_round(rnd, path, state)
        print(f"iter {it}: {msg}")
        if not ok:
            print("FAIL, stopping.", f"(seed={seed}, iteration={it})")
            sys.exit(1)
        passed += 1
    print(f"PASS {passed}/{iters}")


if __name__ == "__main__":
    main()
