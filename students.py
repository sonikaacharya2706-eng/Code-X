"""Student records stored in kvdb.

Each student becomes 3 keys that share an id:
    student:0001:name   -> gaurav
    student:0001:branch -> ise
    student:0001:year   -> 2026-2028
A counter key (meta:next_id) hands out ids. All 4 keys are written in ONE batch,
so a student is saved completely or not at all, even if the program is killed.

Usage:
    python students.py add "Gaurav" ise 2026-2028
    python students.py list
    python students.py find ise          (search by name, branch or year)
    python students.py delete 0001
"""
import sys
from kvdb import DB

DB_PATH = "students.db"


def load(db):
    """Group the flat keys back into {id: {name, branch, year}}."""
    out = {}
    for k, v in sorted(db.index.items()):
        parts = k.decode().split(":")
        if parts[0] == "student":
            out.setdefault(parts[1], {})[parts[2]] = v.decode()
    return out


def show(sid, f):
    print(f"--- student {sid} ---")
    print("name  ", f.get("name"))
    print("branch", f.get("branch"))
    print("year  ", f.get("year"))


def add(db, name, branch, year):
    n = int(db.get(b"meta:next_id") or b"1")
    sid = f"{n:04d}"
    db.batch([
        ("put", f"student:{sid}:name".encode(), name.encode()),
        ("put", f"student:{sid}:branch".encode(), branch.encode()),
        ("put", f"student:{sid}:year".encode(), year.encode()),
        ("put", b"meta:next_id", str(n + 1).encode()),
    ])
    print("added student", sid)


def main(a):
    if not a:
        print(__doc__)
        return
    db = DB(DB_PATH)
    cmd = a[0]
    if cmd == "add" and len(a) == 4:
        add(db, a[1], a[2], a[3])
    elif cmd == "list":
        data = load(db)
        if not data:
            print("no students yet")
        for sid, f in data.items():
            show(sid, f)
    elif cmd == "find" and len(a) == 2:
        word = a[1].lower()
        hits = {s: f for s, f in load(db).items()
                if any(word in x.lower() for x in f.values())}
        if not hits:
            print("no match")
        for sid, f in hits.items():
            show(sid, f)
    elif cmd == "delete" and len(a) == 2:
        sid = a[1]
        if sid not in load(db):
            print("no such student", sid)
        else:
            db.batch([("del", f"student:{sid}:{x}".encode(), b"")
                      for x in ("name", "branch", "year")])
            print("deleted student", sid)
    else:
        print(__doc__)
    db.close()


if __name__ == "__main__":
    main(sys.argv[1:])
