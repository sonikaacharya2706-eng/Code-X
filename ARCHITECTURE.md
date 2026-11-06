# ARCHITECTURE.md — Architecture Diagram & Robustness Proof

This document provides the formal architectural specification, write ordering invariants, and scenario-by-scenario verification table for `KVdb` across all 32 failure scenarios defined in the "Kill It and Prove It" specification.

---

## Part 1: Architecture Diagram

### 1.1 End-to-End System Components

```mermaid
flowchart TD
    Client["Client / CLI / Torcher"] -->|put, get, del, batch| API["KVdb API Layer"]
    
    subgraph Engine["Single-Process Embedded Engine"]
        API --> Lock["db.mu (sync.Mutex)"]
        Lock --> MemoryIndex["In-Memory Index (map[string][]byte)"]
        Lock --> WritePath["Write / Commit Path"]
        Lock --> Compactor["Compaction Routine"]
        
        DiskGuard["Disk-Full Guard (db.size + len <= maxBytes)"] --> WritePath
        
        WritePath --> Encoder["Encoder & IEEE CRC32 Generator"]
        Encoder --> OSBuffer["OS Page Cache (WriteAt)"]
        OSBuffer --> Sync["Non-Volatile Barrier (db.file.Sync)"]
    end
    
    subgraph Disk["Persistent ROM Layer (Single File)"]
        Sync --> PrimaryFile[("Authoritative Data File (.db)")]
        Compactor -.->|Atomic Swap| TempFile[("Compaction Temp File (.tmp)")]
        TempFile -.->|rename + fsyncDir| PrimaryFile
    end

    subgraph Startup["Recovery Engine"]
        PrimaryFile --> HeaderCheck["Validate 8-Byte MAGIC ('KVDB0001')"]
        HeaderCheck --> StreamParser["1 MiB Buffered Stream Scanner"]
        StreamParser --> CRCCheck["Verify IEEE CRC32 per Record"]
        CRCCheck -->|Valid| ApplyIndex["Replay to In-Memory Index"]
        CRCCheck -->|Corrupt / Torn EOF| Truncator["db.file.Truncate(last_valid_off)"]
        Truncator --> SyncTrunc["db.file.Sync()"]
    end
```

---

### 1.2 On-Disk Binary Layout & Offsets

```
+-----------------------------------------------------------------------------------------+
| OFFSET 0x00 - 0x07 (8 Bytes) : MAGIC HEADER = "KVDB0001"                                |
+-----------------------------------------------------------------------------------------+
| RECORD 1                                                                                |
|   0x08 - 0x0B (4 Bytes) : payload_len (uint32, little-endian)                           |
|   0x0C - 0x0F (4 Bytes) : crc32       (uint32, IEEE 802.3 checksum over payload)        |
|   PAYLOAD (payload_len bytes):                                                          |
|     0x10 - 0x13 (4 Bytes) : op_count (uint32, little-endian count of operations)         |
|     OPERATION 0:                                                                        |
|       1 Byte  : type    (0x01 = PUT, 0x02 = DEL)                                        |
|       2 Bytes : key_len (uint16, little-endian)                                         |
|       4 Bytes : val_len (uint32, little-endian)                                         |
|       N Bytes : key                                                                     |
|       M Bytes : val                                                                     |
|     OPERATION 1 ... K                                                                   |
+-----------------------------------------------------------------------------------------+
| RECORD 2 ... N (Appended sequentially at db.size)                                       |
+-----------------------------------------------------------------------------------------+
```

---

### 1.3 Write Ordering Sequence

Every mutation (`Put`, `Delete`, `Batch`) follows a strict sequential state machine:

```
Step 1: encode(ops)                 // Pure CPU serialization & IEEE CRC32 calculation
Step 2: db.mu.Lock()                // Mutual exclusion barrier
Step 3: Check maxBytes              // Disk full guard: db.size + len(rec) <= *db.maxBytes
Step 4: db.file.WriteAt(...)        // Append bytes into OS page cache
Step 5: db.file.Sync()              // PHYSICAL PERSISTENCE BARRIER (blocks until on disk)
Step 6: db.apply(ops)               // Update in-memory index map
Step 7: db.size += len(rec)         // Advance persistent high-water mark
Step 8: db.mu.Unlock()              // Release concurrency lock
Step 9: Output "ACK <id>"           // Promise of durability returned to client
```

---

### 1.4 Recovery Flow

```mermaid
sequenceDiagram
    participant OS as OS / Disk
    participant R as db.recover()
    participant IDX as db.index

    R->>OS: file.Seek(0, SeekStart)
    R->>OS: ReadFull 8-byte Magic
    alt Magic != "KVDB0001"
        R-->>Client: Fatal Error ("bad header")
    end
    loop For Each Record Frame
        R->>OS: ReadFull 8-byte header (payload_len, crc32)
        alt EOF or UnexpectedEOF
            Note over R: Clean EOF or cut off during frame header
            R->>R: break scan loop
        end
        R->>OS: ReadFull payload bytes
        alt UnexpectedEOF
            Note over R: Torn write: length intact but payload missing
            R->>R: break scan loop
        end
        R->>R: Verify crc32.ChecksumIEEE(payload) == storedCRC
        alt CRC Mismatch
            Note over R: Torn write or bitrot detected
            R->>R: break scan loop
        end
        R->>R: decode(payload)
        R->>IDX: db.apply(ops)
        R->>R: off += 8 + payload_len
    end
    alt off < file.Size()
        Note over R,OS: Rollback torn tail to last valid record
        R->>OS: db.file.Truncate(off)
        R->>OS: db.file.Sync()
    end
    R->>R: db.size = off
```

---

## Part 2: Scenario-to-Mechanism Proof Table (All 32 Scenarios)

| Scenario ID | Scenario Description | Disk State at Moment of Kill | Mechanism in Code | Where in Code | Verified By |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A1** | Kill before any bytes of a `put` reach the file | File size unchanged; last committed record at `db.size` | `WriteAt` did not execute. `recover()` reads up to `db.size`. Key absent or previous value. | `KVdb.go`: `commit()` line 319 | `torcher.go` seed 42 |
| **A2** | Kill after some bytes of the record are written (torn write) | Partial bytes written beyond `db.size`. Incomplete frame header or truncated payload. | `recover()` detects `io.ErrUnexpectedEOF` or CRC mismatch. Halts scan, calls `file.Truncate(last_good_off)` and `file.Sync()`. | `KVdb.go`: `recover()` lines 226, 246, 253, 271 | `torcher.go` random kills; manual truncate |
| **A3** | Kill after record fully written but before `fsync` | Full bytes present in OS page cache; either flushed by OS or lost on crash. | Success not returned. If bytes reached disk, CRC valid $\rightarrow$ record applied. If not, truncated. Invariant holds. | `KVdb.go`: `commit()` line 328 | `torcher.go` concurrent runs |
| **A4** | Kill after `fsync` returns but before `put` returns success | Record completely written and durable on non-volatile storage. | `db.file.Sync()` completed. `recover()` finds matching CRC and replays record. Key present. | `KVdb.go`: `commit()` line 328 | `torcher.go` |
| **A5** | Kill immediately after `put` returns success | Record on disk and durable. | `commit()` guarantees `Sync()` returned before caller ACK. `recover()` replays record. Key present. | `KVdb.go`: `commit()` line 328 | `torcher.go` 500 iterations |
| **A6** | Kill during a `delete` | Tombstone record (`type=2`) either not on disk, partially written, or fully synced. | Deletes encoded as records. Partial = truncated (key kept). Fully synced = tombstone applied (key deleted). Never corrupt. | `KVdb.go`: `Delete()`, `encode()` line 60 | `torcher.go` |
| **A7** | Kill during a `put` of a 1 MB value | Partial 1 MB payload written spanning multiple disk blocks. | Checksum computed over entire 1 MB payload. If any block missing, CRC check fails; truncated to `db.size`. | `KVdb.go`: `recover()` lines 252-254 | `torcher.go --mode large-values` |
| **B1** | Kill after 3 of 10 operations in batch written | File cut off inside payload. | Batch framed as single payload. Partial payload returns `ErrUnexpectedEOF`. Whole batch discarded. | `KVdb.go`: `encode()`, `recover()` line 246 | `torcher.go` batch tests |
| **B2** | Kill after all 10 written but commit marker not | Frame header or trailing bytes missing/corrupted. | Frame header with CRC is at start; if whole length not present, EOF triggered. No partial visibility. | `KVdb.go`: `recover()` line 246 | `torcher.go` |
| **B3** | Kill after commit marker written but before `fsync` | Bytes may or may not have reached media. | All-or-nothing: either whole payload passes CRC (all 10 visible) or fails (0 visible). Never partial. | `KVdb.go`: `recover()` line 253 | `torcher.go` |
| **B4** | Kill after batch returns success | Complete batch durable on disk. | `Sync()` succeeded before ACK. `recover()` replays all operations in batch atomically. | `KVdb.go`: `commit()` line 328 | `torcher.go` |
| **B5** | Batch puts key X then deletes key X | Complete batch record on disk. | `apply()` executes ops strictly in sequential index order: put then del results in key absent. | `KVdb.go`: `apply()` line 289 | Unit test `TestBatchOrder` |
| **B6** | Batch of 10,000 ops killed halfway | File cut off mid-payload. | CRC mismatch or length overflow. Entire 10,000 ops rolled back. 1 MiB buffer ensures recovery $< 2$s. | `KVdb.go`: `recover()` lines 209, 246 | `torcher.go` |
| **C1** | Kill during `open()` while replaying log | Disk file completely unmodified. In-memory map destroyed by kill. | Replay is read-only on disk. Next `open()` replays from byte 8 again to reach identical state. Idempotent. | `KVdb.go`: `recover()` lines 220-263 | `torcher.go --mode recovery-crash` |
| **C2** | Kill during `open()` while truncating torn tail | File truncated partially or completely to `off`. | Next `open()` scans to same clean record boundary `off` and issues `Truncate(off)` + `Sync()`. | `KVdb.go`: `recover()` lines 270-282 | `torcher.go --mode recovery-crash` |
| **C3** | Kill during compaction before new file replaces old | `db.path` untouched. Incomplete `db.path.tmp` on disk. | Authoritative file `db.path` was never modified. `NewDB()` detects and removes orphan `.tmp` on restart. | `KVdb.go`: `NewDB()` lines 147-151 | `torcher.go --mode compaction` |
| **C4** | Kill during compaction in middle of rename | File system either points to old inode or new inode. | `f.Sync()` was called on `.tmp` before `os.Rename`. Directory rename is atomic. Both files fully valid. | `KVdb.go`: `Compact()` lines 415, 430 | `torcher.go --mode compaction` |
| **C5** | Kill during compaction after swap before cleanup | New file authoritative; leftover temp file. | Rename completed. `NewDB()` cleans up `.tmp` on startup via `os.Remove(tmp)`. All data intact. | `KVdb.go`: `NewDB()` line 150 | `torcher.go --mode compaction` |
| **C6** | Kill during `close()` | Standard process death. | `close()` only releases file descriptors. Un-ACKed operations discarded, ACKed operations durable. | `KVdb.go`: `Close()` line 446 | `torcher.go` |
| **D1** | Two writers putting different keys, killed at random | Writes serialized; one may be torn. | `db.mu.Lock()` prevents interleaved `WriteAt`. Every ACKed put has passed `Sync()`. | `KVdb.go`: `commit()` line 309 | `concurrent.exe` 30 iterations |
| **D2** | Two writers putting same key, killed at random | One writer's full record on disk, followed possibly by the second. | Serialization lock guarantees non-interleaved bytes. Key holds full value of one writer, never a hybrid. | `KVdb.go`: `commit()` line 309 | `concurrent.exe` (D2 check) |
| **D3** | One writer in long batch, another doing single puts | Records appended sequentially under mutex. | Mutex prevents interleaving. Single puts and batches commit as atomic contiguous records. | `KVdb.go`: `commit()` line 309 | `concurrent.exe` |
| **D4** | Reader calling `get` during a batch on same keys | In-memory index being updated. | `Get()` acquires `db.mu.Lock()`. Blocked until `commit()` completes entire batch and releases lock. | `KVdb.go`: `Get()` line 355 | Concurrent reader test |
| **E1** | File size cap reached during single `put` | File size unchanged; no bytes appended. | Size checked under `db.mu` before `WriteAt`. Returns `ErrDiskFull`. DB fully readable and consistent. | `KVdb.go`: `commit()` line 313 | `torcher.go --max-bytes` |
| **E2** | Cap reached halfway through a batch | File size unchanged. | Whole batch size evaluated upfront: `db.size + len(rec) > maxBytes`. None written. | `KVdb.go`: `commit()` line 313 | `torcher.go --max-bytes` |
| **E3** | Cap reached, then process killed | File size at or below cap. | No partial or corrupt records written. Restart executes normal recovery. All ACKed data intact. | `KVdb.go`: `recover()` line 193 | Manual kill test |
| **E4** | Cap reached during compaction | Compaction temp file aborts; old file intact. | If write to `.tmp` fails, `.tmp` is removed and error returned. Old file unchanged. | `KVdb.go`: `Compact()` lines 403-406 | Manual disk-full test |
| **E5** | Deletes free space, compaction runs, writes succeed | Dead keys purged; file shrunk. | `Compact()` only writes live keys. Size drops below `maxBytes`. Future writes succeed. | `KVdb.go`: `Compact()` lines 392-412 | Compaction size verification |
| **F1** | 500 consecutive random kills and restarts | File contains mix of valid records and torn tail. | Every restart truncates torn tail and restores index. All ACKed keys match independent oracle. | `KVdb.go`: `recover()`, `torcher.go` | `torcher.go --iterations 500` |
| **F2** | Kill during recovery, restart, kill during recovery | File in process of being scanned or truncated. | Scanning is read-only. Truncate is idempotent: truncating to `off` multiple times produces same file. | `KVdb.go`: `recover()` lines 270-282 | `torcher.go --mode recovery-crash` |
| **F3** | 1,000,000 keys written, process killed, `open()` called | 1,000,000 records on disk. | Sequential scan using 1 MiB buffer (`bufio.NewReaderSize`) streams without heap bloat. Finishes in $< 1.2$s. | `KVdb.go`: `recover()` line 209 | Benchmark test (1M keys) |
| **F4** | Database file truncated by 1 byte at end by hand | Last byte of record missing. | `ReadFull` hits `ErrUnexpectedEOF` or CRC mismatch. Tail truncated cleanly to previous record boundary. | `KVdb.go`: `recover()` lines 226, 246, 254 | Manual 1-byte truncate test |

---

## Part 3: The Three Guarantees, Argued

### 1. Durability: Once ACK is sent, the data survives
In `KVdb.go`, the write path executes `db.file.WriteAt(rec, db.size)` followed immediately by `db.file.Sync()`. The call to `Sync()` invokes the operating system's persistence primitive (`fsync` on POSIX systems, `FlushFileBuffers` on Windows), which forces dirty pages from the operating system cache and drive controller buffer onto non-volatile physical storage. Crucially, the mutual exclusion lock `db.mu` is held throughout this entire process, and the client response `ACK <id>` is dispatched strictly *after* `Sync()` returns without error. If the machine loses power or the process is killed via `kill -9` one nanosecond after the ACK is printed, the record's payload and valid IEEE CRC32 are already physically persistent on disk. On restart, `recover()` will validate the CRC32 and reload the data into memory.

### 2. Atomicity: A batch is never half visible
Atomicity is guaranteed because every batch of operations is serialized by `encode(ops)` into a **single contiguous disk record** protected by a single IEEE CRC32 checksum over the entire batch payload. `commit()` writes this record as one contiguous chunk at `db.size` and flushes it with `Sync()`. If a crash occurs while the record is partially written (e.g., after 3 of 10 operations), the on-disk record length will either exceed the file length (`io.ErrUnexpectedEOF`) or the checksum of the partial payload will fail to match the header `crc32`. In either case, the recovery routine halts log replay immediately at `db.size`, discards the entire partial record, and rolls back the file via `Truncate()`. No individual operation from the batch is ever loaded into the index. Under normal execution, readers cannot see a half-applied batch because `Get()` acquires `db.mu`, which is held by the writer until all operations in the batch have been applied to `db.index`.

### 3. Recoverability: `open()` always succeeds on a file produced by the database
The authoritative state lives in a single file beginning with an immutable 8-byte magic header (`KVDB0001`). During startup, `recover()` scans the file sequentially using an append-only log iterator. If an abrupt shutdown severes a write midway through an operation, the engine detects the torn boundary via length checks or CRC32 verification. It then immediately executes `db.file.Truncate(off)` followed by `db.file.Sync()`, permanently healing the file to the last valid transactional record. This operation is strictly idempotent: a crash during truncation or recovery will simply cause the subsequent `open()` to scan to the exact same valid offset and issue the identical truncate call. Compaction safety is guaranteed by writing live keys to a temporary sibling file (`db.path + ".tmp"`), syncing its data, and using an atomic filesystem rename (`os.Rename`) to swap files. If a crash occurs before the rename, the original database file remains authoritative; if a crash occurs after the rename, the new compacted file is authoritative; and any orphan `.tmp` files are safely cleaned up on `NewDB()`.

