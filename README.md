# KVdb - Crash-Safe Key-Value Storage Engine

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

Now kill the recovery three times:
for %d in (5 15 30) do (copy /y t\k.db t\r.db >nul & start /b kvdb.exe open t\r.db & powershell -c "Start-Sleep -Milliseconds %d" & taskkill /F /IM kvdb.exe)

## Part 3: 
Recover for real and verify

kvdb.exe open t\r.db
kvdb.exe check t\r.db
find /c "ACK" t\acks.txt
powershell -c "Get-Content t\acks.txt -Tail 1"

## Part 4: 
The PDF’s F2 sequence (kill, restart, kill during recovery, restart, kill during recovery again, restart)

copy /y t\k.db t\f2.db
start /b kvdb.exe open t\f2.db & powershell -c "Start-Sleep -Milliseconds 10" & taskkill /F /IM kvdb.exe
start /b kvdb.exe open t\f2.db & powershell -c "Start-Sleep -Milliseconds 10" & taskkill /F /IM kvdb.exe
kvdb.exe open t\f2.db




## 1. Our Code

### Project Files

| File | Purpose |
| :--- | :--- |
| `KVdb.go` | The storage engine library: record encoding and decoding, commit path, recovery, compaction |
| `kvdb_cli.go` | Command line program: `open`, `put`, `get`, `del`, `batch`, `dump`, `check`, `serve` |
| `torcher.go` | Torture test: starts `kvdb serve`, sends random operations, kills it with SIGKILL, reopens the database and checks it against an oracle |
| `torture.go` | Identical copy of `torcher.go` |
| `concurrent.go` | Concurrency test: two writer goroutines race on the same database, the process is killed, and durability is verified |
| `studentdb.go` | Example application: student records stored on top of KVdb using atomic batches |
| `verify_32_scenarios.ps1` | PowerShell script that checks all 32 crash and integrity scenarios |
| `ARCHITECTURE.md` | Architecture diagram and robustness proof |
| `BREAKING.md` | Crash point analysis and known weaknesses |
| `METADATA.md` | On-disk format specification |
| `PROMPTS.md` | AI prompt log required by the competition rules |

### What the Engine Provides

| Feature | How it works |
| :--- | :--- |
| Durable writes | Each write is appended to the file and fsynced before success is reported |
| Atomic batches | Many `put` and `del` operations are stored as one framed record, so they are applied all together or not at all |
| Tombstone deletes | A delete is an appended record, not an in-place edit |
| Corruption detection | Every record carries an IEEE CRC32 checksum over its payload |
| Torn write recovery | On open, the file is scanned and truncated back to the last valid record |
| Disk-full guard | With `--max-bytes N`, a write that would exceed the limit is rejected and nothing is written |
| Compaction | Rewrites the file with only live keys, then swaps it in using an atomic rename |
| Thread safety | One global mutex protects the index, the file and the size |

### Engine API (`KVdb.go`)

| Function | Description |
| :--- | :--- |
| `NewDB(path, maxBytes)` | Opens or creates the database and runs recovery |
| `Put(key, value)` | Durably stores a key and value |
| `Delete(key)` | Durably removes a key |
| `Batch(ops)` | Atomically applies a list of put and delete operations |
| `Get(key)` | Returns a copy of the value, or nil if the key is missing |
| `Compact()` | Rewrites the file with only live keys |
| `Close()` | Closes the file |

---

## 2. Architecture

### System Overview

```text
     +------------------------+        +------------------------------+
     |      CLI commands      |        |          serve mode          |
     | open put get del batch |        | stdin lines, ACK after fsync |
     |      dump check        |        |                              |
     +-----------+------------+        +---------------+--------------+
                 |                                     |
                 +------------------+------------------+
                                    |
                                    v
                 +--------------------------------------+
                 |   KVdb.go engine (global mutex)      |
                 +------+---------------------+---------+
                        |                     |
                append and fsync        reads and updates
                        |                     |
                        v                     v
            +---------------------+   +----------------------+
            |  Single data file   |   |  In-memory index     |
            | header + CRC records|   |  map: key -> value   |
            +----------+----------+   +----------------------+
                       |                         ^
                  read on open                   |
                       v                         |
            +---------------------+              |
            |      Recovery       |--------------+
            | verify, cut bad tail|  rebuild index
            +---------------------+
```

### Write Path

```text
   +------------------------------------------------+
   | Encode all ops into ONE record                 |
   | [ payload_len | crc32 | payload ]              |
   +-----------------------+------------------------+
                           |
                           v
   +------------------------------------------------+
   | Lock db.mu                                     |
   +-----------------------+------------------------+
                           |
                           v
   +------------------------------------------------+
   | size + record length > max bytes ?             |
   +-----------+------------------------+-----------+
               |                        |
              YES                       NO
               |                        |
               v                        v
   +---------------------+   +-----------------------------+
   | Return ErrDiskFull  |   | WriteAt record at end       |
   | Write nothing       |   | of file                     |
   +---------------------+   +--------------+--------------+
                                            |
                                            v
                             +-----------------------------+
                             | fsync                       |
                             +--------------+--------------+
                                            |
                                            v
                             +-----------------------------+
                             | Apply ops to in-memory      |
                             | index and grow db.size      |
                             +--------------+--------------+
                                            |
                                            v
                             +-----------------------------+
                             | Unlock and return success   |
                             | serve prints ACK            |
                             +-----------------------------+
```

### Recovery Path

```text
   +------------------------------------------------+
   | Delete leftover path.tmp file                  |
   +-----------------------+------------------------+
                           |
                           v
   +------------------------------------------------+
   | Database file exists ?                         |
   +-----------+------------------------+-----------+
               |                        |
              YES                       NO
               |                        |
               |                        v
               |            +-----------------------------+
               |            | Create tmp with magic       |
               |            | header, fsync, rename       |
               |            +--------------+--------------+
               |                           |
               +-------------+-------------+
                             |
                             v
   +------------------------------------------------+
   | Check magic header "KVDB0001"                  |
   +-----------------------+------------------------+
                           |
                           v
      +------------------------------------------------+
  +-->| Read next record: header and payload           |
  |   +-----------------------+------------------------+
  |                           |
  |                           v
  |   +------------------------------------------------+
  |   | Record complete, crc ok and decodes ?          |
  |   +-----------+------------------------+-----------+
  |               |                        |
  |              YES                       NO
  |               |                        |
  |               v                        v
  |   +---------------------+   +-----------------------------+
  |   | Replay record into  |   | Truncate file to last good  |
  |   | in-memory index     |   | offset and fsync            |
  |   +----------+----------+   | (skipped on clean end)      |
  |              |              +--------------+--------------+
  +--------------+                             |
                                               v
                                     +------------------+
                                     |  Database ready  |
                                     +------------------+
```

### Compaction Path

```text
   +-----------------------------------------------+
   | Lock db.mu                                    |
   +-----------------------+-----------------------+
                           |
                           v
   +-----------------------------------------------+
   | Write magic header and all live keys          |
   | to path.tmp (batches of 1000)                 |
   +-----------------------+-----------------------+
                           |
                           v
   +-----------------------------------------------+
   | fsync tmp file                                |
   +-----------------------+-----------------------+
                           |
                           v
   +-----------------------------------------------+
   | new size > max bytes ?                        |
   +-----------+-----------------------+-----------+
               |                       |
              YES                      NO
               |                       |
               v                       v
   +---------------------+   +----------------------------+
   | Remove tmp file     |   | Rename tmp over database   |
   | Return ErrDiskFull  |   | file, then fsync directory |
   +---------------------+   +-------------+--------------+
                                           |
                                           v
                             +----------------------------+
                             | Open new file descriptor,  |
                             | then close the old one     |
                             +-------------+--------------+
                                           |
                                           v
                             +----------------------------+
                             | Unlock and return success  |
                             +----------------------------+
```

### Layers

**Client layer** (`kvdb_cli.go`) parses commands and calls the engine. Reads (`get`, `dump`, `check`) are served from the in-memory index. In `serve` mode every input line starts with an operation id, and `ACK <id>` is printed only after the write has been fsynced.

**Write path** (`commit` in `KVdb.go`) works in this order:

1. Encode all operations into one record.
2. Take the global mutex.
3. Reject the write if `size + record length` is greater than `--max-bytes`.
4. Append the record at the end of the file.
5. Call `fsync`.
6. Update the in-memory index and the file size.
7. Return success, and the caller prints the ACK.

If the write or the fsync fails, the file is truncated back to its last committed size.

**In-memory index** is a `map[string][]byte` that mirrors the live data. It is rebuilt from the file every time the database opens.

**Disk file** is a single append-only log. It starts with an 8-byte magic header, followed by CRC-framed records.

**Recovery** runs on every open. It checks the magic header, reads records through a 1 MiB buffered reader, verifies each CRC32, and replays valid records into the index. On the first torn header, short payload, CRC mismatch or malformed record, it stops, truncates the file to the last valid offset, and fsyncs the truncation. Any leftover `.tmp` file from an interrupted compaction is deleted first.

**Compaction** writes only live keys into `path.tmp` in batches of 1000, fsyncs it, atomically renames it over the database file, fsyncs the directory, and then swaps the file descriptor. The new descriptor is opened before the old one is closed. If the compacted file would exceed `--max-bytes`, compaction is cancelled.

### On-Disk Format

```text
+-----------------------------------------------------------------------------+
| HEADER (8 bytes): "KVDB0001"                                                |
+-----------------------------------------------------------------------------+
| RECORD                                                                      |
|   payload_len : 4 bytes, uint32, little-endian                              |
|   crc32       : 4 bytes, uint32, IEEE checksum of the payload               |
|   PAYLOAD (payload_len bytes)                                               |
|     op_count  : 4 bytes, uint32, little-endian                              |
|     OPERATION (repeated op_count times)                                     |
|       type    : 1 byte, 0x01 = PUT, 0x02 = DEL                              |
|       key_len : 2 bytes, uint16, little-endian                              |
|       val_len : 4 bytes, uint32, little-endian                              |
|       key     : key_len bytes                                               |
|       value   : val_len bytes                                               |
+-----------------------------------------------------------------------------+
| next RECORD is appended at the end of the file                              |
+-----------------------------------------------------------------------------+
```

### Why It Survives a Kill

| Crash moment | State on disk | What recovery does |
| :--- | :--- | :--- |
| Before any bytes are written | File unchanged | Nothing to fix |
| Partway through the 8-byte record header | Partial header at the end | Truncates it |
| Partway through the payload | Full header, short payload | Truncates it |
| After the write but before fsync | Record may be complete or partial | Keeps it if the CRC is valid, otherwise truncates it |
| After fsync | Complete record | Keeps it, and the write was durable |
| During compaction | Original file untouched, `.tmp` file left over | Deletes `.tmp` on the next open |

Because the ACK is printed only after fsync returns, every acknowledged write is guaranteed to survive a kill.

---

## 3. Prerequisites

Go 1.19 or newer, with `go` available on your PATH. Check it with:

```bash
go version
```

No external packages are needed, and no `go.mod` file is required because every build command lists its source files explicitly.

On Windows, use either Command Prompt or PowerShell. Commands that work in both are shown once, and commands that differ are shown separately for each shell. Run every command from the repository root.

---

## 4. Build

### Linux and macOS

```bash
git clone https://github.com/sonikaacharya2706-eng/Code-X.git
cd Code-X
mkdir -p bin
go build -o bin/kvdb KVdb.go kvdb_cli.go
go build -o bin/torcher torcher.go KVdb.go
go build -o bin/torture torture.go KVdb.go
go build -o bin/concurrent concurrent.go KVdb.go
go build -o bin/students studentdb.go KVdb.go
```

### Windows (Command Prompt or PowerShell)

```
git clone https://github.com/sonikaacharya2706-eng/Code-X.git
cd Code-X
mkdir bin
go build -o bin\kvdb.exe KVdb.go kvdb_cli.go
go build -o bin\torcher.exe torcher.go KVdb.go
go build -o bin\torture.exe torture.go KVdb.go
go build -o bin\concurrent.exe concurrent.go KVdb.go
go build -o bin\students.exe studentdb.go KVdb.go
```

Each program has its own `main`, so each one is built separately together with `KVdb.go`. Do not pass two program files to the same build command.

---

## 5. CLI Commands

The first argument is the command, and the second argument is always the database path. If the file does not exist, it is created.

### Linux and macOS

Open the database and print the key count and recovery time:

```bash
./bin/kvdb open data.db
```

Store, read and delete a key:

```bash
./bin/kvdb put data.db name Gaurav
./bin/kvdb get data.db name
./bin/kvdb get data.db missing_key
./bin/kvdb del data.db name
```

Apply a batch from a file. Each line is `put <key> <value>` or `del <key>`. Blank lines and lines starting with `#` are skipped:

```bash
printf 'put city Bangalore\nput lang Go\ndel lang\n' > ops.txt
./bin/kvdb batch data.db ops.txt
```

Print all keys in sorted order and verify integrity:

```bash
./bin/kvdb dump data.db
./bin/kvdb check data.db
```

Run in server mode. Each line is `<op-id> <command>`, and the server prints `ACK <op-id>` only after the data is fsynced:

```bash
printf '1 put k1 v1\n2 put k2 v2\n3 del k1\n4 batch put:a:1 put:b:2 del:k2\n5 compact\n' | ./bin/kvdb serve data.db
```

Test the disk-full guard:

```bash
echo "1 put big this-value-is-longer-than-the-limit" | ./bin/kvdb serve limited.db --max-bytes 30
```

### Windows: basic commands (Command Prompt or PowerShell)

```
.\bin\kvdb.exe open data.db
.\bin\kvdb.exe put data.db name Gaurav
.\bin\kvdb.exe get data.db name
.\bin\kvdb.exe get data.db missing_key
.\bin\kvdb.exe del data.db name
```

### Windows: batch from a file

Command Prompt:

```
>ops.txt echo put city Bangalore
>>ops.txt echo put lang Go
>>ops.txt echo del lang
.\bin\kvdb.exe batch data.db ops.txt
```

PowerShell:

```powershell
Set-Content -Path ops.txt -Value @("put city Bangalore","put lang Go","del lang")
.\bin\kvdb.exe batch data.db ops.txt
```

### Windows: dump and check (Command Prompt or PowerShell)

```
.\bin\kvdb.exe dump data.db
.\bin\kvdb.exe check data.db
```

### Windows: server mode

Command Prompt:

```
>serve_input.txt echo 1 put k1 v1
>>serve_input.txt echo 2 put k2 v2
>>serve_input.txt echo 3 del k1
>>serve_input.txt echo 4 batch put:a:1 put:b:2 del:k2
.\bin\kvdb.exe serve data.db < serve_input.txt
```

PowerShell:

```powershell
@("1 put k1 v1","2 put k2 v2","3 del k1","4 batch put:a:1 put:b:2 del:k2") | .\bin\kvdb.exe serve data.db
```

### Windows: disk-full guard

Command Prompt:

```
>big.txt echo 1 put big this-value-is-longer-than-the-limit
.\bin\kvdb.exe serve limited.db --max-bytes 30 < big.txt
```

PowerShell:

```powershell
"1 put big this-value-is-longer-than-the-limit" | .\bin\kvdb.exe serve limited.db --max-bytes 30
```

### Command Reference

| Command | Description |
| :--- | :--- |
| `open <path> [--max-bytes N]` | Opens or creates the database, runs recovery, prints key count and recovery time |
| `put <path> <key> <value>` | Stores a value durably |
| `get <path> <key>` | Prints the value, or `NOTFOUND` |
| `del <path> <key>` | Deletes a key by appending a tombstone |
| `batch <path> <file>` | Atomically applies the operations listed in the file |
| `dump <path>` | Prints every key and value, sorted by key |
| `check <path>` | Verifies file integrity and key count |
| `serve <path> [--max-bytes N]` | Reads `<id> <command>` lines from stdin and prints `ACK <id>` after each durable success |

### Serve Mode Commands

| Input line | Meaning |
| :--- | :--- |
| `<id> put <key> <value>` | Store a value, and the value may contain spaces |
| `<id> del <key>` | Delete a key |
| `<id> batch put:<key>:<value> del:<key> ...` | Atomic batch, with keys and values URL-escaped, for example `%20` for a space |
| `<id> compact` | Compact the database file, currently supported on Linux and macOS only |

Replies are `ACK <id>` on success, `ERR <id> diskfull` when the size limit is hit, and `ERR <id> <reason>` for any other failure.

---

## 6. Student Records Demo

`studentdb.go` is a small application built on KVdb. Each student is saved as three keys (`student:<id>:name`, `student:<id>:branch`, `student:<id>:year`), and a counter key `meta:next_id` hands out ids. All of these keys are written in one atomic batch, so a student record is saved completely or not at all, even if the program is killed. Records are stored in `students.db`.

### Linux and macOS

```bash
./bin/students add "Gaurav" ise 2026-2028
./bin/students add "Asha" cse 2026-2028
./bin/students list
./bin/students find ise
./bin/students delete 0001
./bin/students list
```

### Windows (Command Prompt or PowerShell)

```
.\bin\students.exe add "Gaurav" ise 2026-2028
.\bin\students.exe add "Asha" cse 2026-2028
.\bin\students.exe list
.\bin\students.exe find ise
.\bin\students.exe delete 0001
.\bin\students.exe list
```

---

## 7. Kill and Verify Tests

Both tests need the `kvdb` binary from the build step. The torture test starts it itself, so pass its location with `--bin`.

### Torture Test

The torture test repeatedly starts `kvdb serve`, streams random `put` and `del` operations over 30 keys, kills the process with SIGKILL after a random delay, reopens the database, and checks every key against an oracle. A key must hold either its last acknowledged value or a value from a later operation that was in flight when the kill happened. It also checks that no unknown keys appear.

Linux and macOS:

```bash
./bin/torcher --iterations 100 --seed 12345 --bin ./bin/kvdb
./bin/torture --iterations 100 --seed 1 --bin ./bin/kvdb
```

Windows (Command Prompt or PowerShell):

```
.\bin\torcher.exe --iterations 100 --seed 12345 --bin .\bin\kvdb.exe
.\bin\torture.exe --iterations 100 --seed 1 --bin .\bin\kvdb.exe
```

| Flag | Default | Meaning |
| :--- | :--- | :--- |
| `--iterations N` | 100 | Number of kill and verify rounds |
| `--seed N` | 1 | Random seed, so a failing run can be repeated exactly |
| `--bin PATH` | `./kvdb.exe` | Path to the `kvdb` binary |

### Concurrent Writer Test

This test runs two writers inside one child process. Each writer puts its own keys and both writers also fight over a key named `shared`. The parent kills the child after 200 to 700 ms, reopens the database, and checks that every acknowledged key exists with the correct value and that `shared` holds a value that was really written.

Linux and macOS:

```bash
./bin/concurrent --iterations 30 --seed 1
```

Windows (Command Prompt or PowerShell):

```
.\bin\concurrent.exe --iterations 30 --seed 1
```

---

## 8. Full Scenario Suite (Windows)

`verify_32_scenarios.ps1` runs all 32 crash and integrity scenarios, including torn writes, corrupted checksums, disk-full, compaction and recovery. It looks for `kvdb.exe`, `concurrent.exe` and `torcher.exe` in the repository root, so build them there first and then run the script from the repository root.

```
go build -o kvdb.exe KVdb.go kvdb_cli.go
go build -o torcher.exe torcher.go KVdb.go
go build -o concurrent.exe concurrent.go KVdb.go
powershell -ExecutionPolicy Bypass -File .\verify_32_scenarios.ps1
```

---

## 9. Clean Up

Linux and macOS:

```bash
rm -rf bin data.db limited.db students.db ops.txt
```

Windows Command Prompt:

```
rmdir /s /q bin
rmdir /s /q test_workspace_32
del /q data.db limited.db students.db ops.txt serve_input.txt big.txt kvdb.exe torcher.exe concurrent.exe
```

Windows PowerShell:

```powershell
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue bin, test_workspace_32, data.db, limited.db, students.db, ops.txt, kvdb.exe, torcher.exe, concurrent.exe
```

---

## Further Reading

| Document | Contents |
| :--- | :--- |
| `ARCHITECTURE.md` | Write ordering invariants and the 32-scenario proof table |
| `BREAKING.md` | Crash points, attack analysis and acknowledged weaknesses |
| `METADATA.md` | Full on-disk format specification |
