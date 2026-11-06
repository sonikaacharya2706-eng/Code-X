"""Mini crash-safe KV store. Append-only log + in-memory index.

FILE FORMAT
  header : 8 bytes  b"KVDB0001"
  record : [4B payload_len][4B crc32(payload)][payload]       (little endian)
  payload: [4B op_count] then per op: [1B type 1=put 2=del][2B klen][4B vlen][key][value]

A batch is ONE record, so it is atomic: a torn record fails length/CRC and is cut off on open.
"""
import os, sys, struct, zlib, threading

MAGIC = b"KVDB0001"
BINARY = getattr(os, "O_BINARY", 0)  # Windows opens files in text mode unless told otherwise
HDR = struct.Struct("<II")


class DiskFull(Exception):
    pass


def _fsync_dir(path):
    # Needed so a rename/create survives. (Works on Linux/macOS; skipped on Windows.)
    try:
        fd = os.open(os.path.dirname(os.path.abspath(path)), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _encode(ops):
    parts = [struct.pack("<I", len(ops))]
    for op, k, v in ops:
        v = v or b""
        parts.append(struct.pack("<BHI", 1 if op == "put" else 2, len(k), len(v)) + k + v)
    payload = b"".join(parts)
    return HDR.pack(len(payload), zlib.crc32(payload)) + payload


def _decode(payload):
    (n,) = struct.unpack_from("<I", payload, 0)
    off, ops = 4, []
    for _ in range(n):
        t, kl, vl = struct.unpack_from("<BHI", payload, off)
        off += 7
        k = payload[off:off + kl]; off += kl
        v = payload[off:off + vl]; off += vl
        ops.append(("put" if t == 1 else "del", k, v))
    return ops


class DB:
    def __init__(self, path, max_bytes=None):
        self.path, self.max = path, max_bytes
        self.lock = threading.Lock()
        self.index = {}
        tmp = path + ".tmp"
        if os.path.exists(tmp):          # leftover from a crashed compaction/create
            os.remove(tmp)
        if not os.path.exists(path):     # create atomically: temp file, fsync, rename
            with open(tmp, "wb") as f:
                f.write(MAGIC); f.flush(); os.fsync(f.fileno())
            os.replace(tmp, path)
            _fsync_dir(path)
        self.fd = os.open(path, os.O_RDWR | BINARY)
        self._recover()

    def _recover(self):
        with open(self.path, "rb") as f:
            data = f.read()
        if data[:8] != MAGIC:
            raise ValueError("bad header")
        off = 8
        while off + 8 <= len(data):
            ln, crc = HDR.unpack_from(data, off)
            end = off + 8 + ln
            if end > len(data):
                break                                    # torn: length runs past EOF
            payload = data[off + 8:end]
            if zlib.crc32(payload) != crc:
                break                                    # torn/garbage: CRC mismatch
            self._apply(_decode(payload))
            off = end
        if off < len(data):                              # chop the bad tail
            os.ftruncate(self.fd, off)
            os.fsync(self.fd)
        self.size = off

    def _apply(self, ops):
        for op, k, v in ops:
            if op == "put":
                self.index[k] = v
            else:
                self.index.pop(k, None)

    def _commit(self, ops):
        rec = _encode(ops)
        with self.lock:
            if self.max is not None and self.size + len(rec) > self.max:
                raise DiskFull("max_bytes reached")      # nothing written, state unchanged
            try:
                done = 0
                while done < len(rec):
                    os.lseek(self.fd, self.size + done, os.SEEK_SET)
                    done += os.write(self.fd, rec[done:])
                os.fsync(self.fd)                        # durable BEFORE we return/ACK
            except OSError:
                os.ftruncate(self.fd, self.size)         # undo partial write
                raise
            self._apply(ops)
            self.size += len(rec)

    def put(self, k, v):
        self._commit([("put", k, v)])

    def delete(self, k):
        self._commit([("del", k, b"")])

    def batch(self, ops):
        self._commit(ops)

    def get(self, k):
        with self.lock:
            return self.index.get(k)

    def compact(self):
        with self.lock:
            tmp = self.path + ".tmp"
            items = list(self.index.items())
            with open(tmp, "wb") as f:
                f.write(MAGIC)
                for i in range(0, len(items), 1000):
                    f.write(_encode([("put", k, v) for k, v in items[i:i + 1000]]))
                f.flush(); os.fsync(f.fileno()); newsize = f.tell()
            os.replace(tmp, self.path)                   # atomic swap
            _fsync_dir(self.path)
            os.close(self.fd)
            self.fd = os.open(self.path, os.O_RDWR | BINARY)
            self.size = newsize

    def close(self):
        os.close(self.fd)


def serve(path, max_bytes):
    """stdin lines: '<id> put <k> <v>' | '<id> del <k>' | '<id> compact'. ACK only after fsync."""
    db = DB(path, max_bytes)
    for line in sys.stdin:
        p = line.rstrip("\n").split(" ", 3)
        try:
            if p[1] == "put":
                db.put(p[2].encode(), p[3].encode())
            elif p[1] == "del":
                db.delete(p[2].encode())
            elif p[1] == "compact":
                db.compact()
            print(f"ACK {p[0]}", flush=True)
        except DiskFull:
            print(f"ERR {p[0]} diskfull", flush=True)


def main(a):
    cmd, path = a[0], a[1]
    mb = int(a[a.index("--max-bytes") + 1]) if "--max-bytes" in a else None
    if cmd == "serve":
        return serve(path, mb)
    db = DB(path, mb)
    if cmd == "put":
        db.put(a[2].encode(), a[3].encode())
    elif cmd == "get":
        v = db.get(a[2].encode())
        print("NOTFOUND" if v is None else v.decode())
    elif cmd == "del":
        db.delete(a[2].encode())
    elif cmd == "dump":
        for k in sorted(db.index):
            print(k.decode(), db.index[k].decode())
    elif cmd == "check":
        print("OK", len(db.index), "keys")
    db.close()


if __name__ == "__main__":
    main(sys.argv[1:])
