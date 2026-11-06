# KVdb — Crash-Safe Key-Value Storage Engine
**"Kill It and Prove It" 24-Hour Engineering Hackathon**

---

## 1. How to Build

From the repository root, build the standalone CLI executable using standard Go toolchains (no external dependencies):

```bash
# On Linux / macOS:
go build -o kvdb KVdb.go kvdb_cli.go

# On Windows (PowerShell):
go build -o kvdb.exe KVdb.go kvdb_cli.go
```

To build all companion verification tools:
```bash
go build -o torcher.exe KVdb.go torcher.go
go build -o concurrent.exe KVdb.go conurrent.go
go build -o students.exe KVdb.go STudentdb.go
```

---

## 2. CLI Command Reference

All commands run against the single persistent database file:

### `open <path> [--max-bytes N]`
Opens or creates the database, runs recovery, and prints key count and recovery time:
```bash
./kvdb open data.db
# Output: OK 10 keys, recovery time: 1.24ms
```

### `put <path> <key> <value>`
Stores the given value under the key (overwriting if it exists) with physical fsync:
```bash
./kvdb put data.db name Gaurav
```

### `get <path> <key>`
Retrieves the value for the given key, or prints `NOTFOUND`:
```bash
./kvdb get data.db name
# Output: Gaurav

./kvdb get data.db missing_key
# Output: NOTFOUND
```

### `del <path> <key>`
Deletes the key by appending an atomic tombstone record:
```bash
./kvdb del data.db name
```

### `batch <path> <file>`
Atomically applies a sequence of `put` and `del` operations from a file (one operation per line):
```bash
# Batch file format:
# put key1 value1
# del key2
./kvdb batch data.db ops.txt
```

### `dump <path>`
Prints all active keys and values in lexicographically sorted order:
```bash
./kvdb dump data.db
```

### `check <path>`
Verifies file structural integrity, checksums, and key count:
```bash
./kvdb check data.db
# Output: OK 10 keys
```

### `serve <path> [--max-bytes N]`
Long-running server reading line-delimited commands from `stdin` and printing `ACK <op-id>` only after durable fsync:
```bash
./kvdb serve data.db
# Input:  1 put city Bangalore
# Output: ACK 1
```

---

## 3. On-Disk Binary Format

All authoritative state lives in a single append-only log file.

```
+-----------------------------------------------------------------------------------------+
| OFFSET 0x00 - 0x07 (8 Bytes) : MAGIC HEADER = "KVDB0001"                                |
+-----------------------------------------------------------------------------------------+
| RECORD 1                                                                                |
|   0x08 - 0x0B (4 Bytes) : payload_len (uint32, Little-Endian)                           |
|   0x0C - 0x0F (4 Bytes) : crc32       (uint32, IEEE 802.3 Checksum over Payload)        |
|   PAYLOAD (payload_len bytes):                                                          |
|     0x10 - 0x13 (4 Bytes) : op_count (uint32, count of operations in this batch)        |
|     OPERATION 0:                                                                        |
|       1 Byte  : type    (0x01 = PUT, 0x02 = DEL)                                        |
|       2 Bytes : key_len (uint16, Little-Endian)                                         |
|       4 Bytes : val_len (uint32, Little-Endian)                                         |
|       N Bytes : key bytes                                                               |
|       M Bytes : value bytes                                                             |
|     OPERATION 1 ... K                                                                   |
+-----------------------------------------------------------------------------------------+
| RECORD 2 ... M (Appended sequentially at db.size)                                       |
+-----------------------------------------------------------------------------------------+
```

---

## 4. Recovery Algorithm (in Plain English)

When `KVdb` opens a file, it assumes the previous process may have been terminated midway through any CPU instruction:

1. **Magic Header Validation:**  
   The engine reads the first 8 bytes. If the bytes do not match the ASCII string `KVDB0001`, `open()` aborts immediately with a fatal error.

2. **Sequential Frame Scan:**  
   Starting at byte offset 8, a 1 MiB buffered stream reader walks the file record by record. For each record:
   * It reads the 8-byte frame header containing `payload_len` and the IEEE `crc32`.
   * If the file terminates cleanly at a record boundary, recovery completes.
   * If reading the frame header encounters an unexpected end-of-file, the engine identifies a torn write and halts scanning.
   * If the record length extends past the physical end of the file, it identifies a torn write and halts scanning.

3. **Checksum Verification:**  
   The engine reads the exact payload bytes into a buffer and computes `crc32.ChecksumIEEE(payload)`. If the computed checksum does not match the header `crc32`, the record was torn, corrupted, or partially flushed by the disk controller. The engine halts scanning immediately.

4. **In-Memory Replay:**  
   Once the record passes CRC validation, its operations are decoded and replayed into the in-memory hash index (`db.index`).

5. **Torn Tail Truncation:**  
   If the scan halted before physical EOF (due to a torn header, truncated payload, or CRC mismatch), the engine permanently truncates the file back to the last valid record offset (`file.Truncate(off)`), followed immediately by `file.Sync()`. This rollback is durable and idempotent.

---

## 5. Running the Torture Test

The torture tool runs consecutive kill cycles using random delays and verifies database recovery against an independent oracle log:

```bash
# 500 iterations with random seed:
./torcher.exe --iterations 500 --seed 12345

# Multi-writer concurrency test:
./concurrent.exe --iterations 30 --seed 1
```

