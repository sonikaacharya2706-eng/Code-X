# BREAKING.md — Attack Analysis & Vulnerability Profile

**Database:** `KVdb` Embedded Key-Value Storage Engine  
**Focus:** Failure point enumeration, ordering proof, acknowledged design weaknesses, and post-mortem analysis.

---

## 1. Crash Points Table

Below is an exhaustive enumeration of 10 points in the write and recovery paths where a sudden process termination (`kill -9` / power cut) interrupts execution:

| Crash Point | Exact Line / Location | What is on Disk if Crash Occurs Here | What `open()` / `recover()` Does About It |
| :--- | :--- | :--- | :--- |
| **CP-01** | `KVdb.go`: `commit()`, before `WriteAt` (line 320) | Pre-existing committed records up to `db.size`. Zero bytes of new record on disk. | `recover()` scans up to `db.size`. Key absent or previous value. File size matches valid offset. No action needed. |
| **CP-02** | `KVdb.go`: `commit()`, inside `WriteAt` loop mid-header (line 322) | Partial 8-byte frame header written ($1\text{ to }7$ bytes). | `recover()` calls `io.ReadFull(r, hdr)`. Returns `io.ErrUnexpectedEOF`. Halts scan, truncates file back to `db.size`, calls `file.Sync()`. |
| **CP-03** | `KVdb.go`: `commit()`, mid-payload write (line 322) | Full 8-byte header on disk, but only partial payload bytes written. | `recover()` reads header (`payloadLen`), attempts `io.ReadFull(r, payload)`. Returns `io.ErrUnexpectedEOF` or fails CRC32. Truncates back to `db.size`. |
| **CP-04** | `KVdb.go`: `commit()`, after all bytes written but before `db.file.Sync()` (line 328) | All bytes present in OS page cache; non-volatile disk may have all, some, or none of the blocks. | If dirty pages reached disk before crash, CRC32 passes $\rightarrow$ record replayed. If blocks missing $\rightarrow$ CRC fails and record truncated. No partial state. |
| **CP-05** | `KVdb.go`: `commit()`, after `db.file.Sync()` returns but before `apply()` (line 337) | Complete, valid, CRC-verified record physically durable on disk. Memory state lost. | `recover()` reads file sequentially, verifies CRC32, and replays operations into `db.index`. Value is preserved even though caller never saw ACK. |
| **CP-06** | `KVdb.go`: `Compact()`, writing to `.tmp` file (line 408) | Incomplete `.tmp` file containing partial key set; original `.db` file completely untouched. | `NewDB()` detects `db.path + ".tmp"`, calls `os.Remove(tmp)`. Original `.db` file remains authoritative. Zero data lost. |
| **CP-07** | `KVdb.go`: `Compact()`, after `tmp.Sync()` but before `os.Rename` (line 430) | Full, valid `.tmp` file durable on disk; original `.db` file also durable and untouched. | `NewDB()` discards `.tmp` and loads original `.db`. All acknowledged data up to the start of compaction is preserved. |
| **CP-08** | `KVdb.go`: `Compact()`, during `os.Rename` (line 430) | Filesystem directory inode swap in flight. | On POSIX/NTFS, rename is atomic. Either old file or new file is linked; neither is half-linked. Startup opens whichever is present. |
| **CP-09** | `KVdb.go`: `recover()`, while reading records (line 244) | Disk file unmodified (recovery only performs memory re-indexing during scan). | Next `open()` replays identical byte log from offset 8. Replay is read-only and strictly idempotent. |
| **CP-10** | `KVdb.go`: `recover()`, during `file.Truncate(off)` (line 271) | File partially truncated or at old length; torn bytes still present at tail. | Next `open()` scans to the exact same clean record boundary `off`, detects the torn tail, and re-executes `Truncate(off)` and `Sync()`. |

---

## 2. The Ordering Argument

Why does our sequence of `write`, `fsync`, and `rename` guarantee the invariant without exception?

1. **Write Path Ordering:**
   $$\text{WriteAt}(rec) \longrightarrow \text{file.Sync}() \longrightarrow \text{apply}(\text{index}) \longrightarrow \text{ACK}$$
   * We issue `db.file.WriteAt()` to append the serialized, CRC-protected frame into OS page buffers.
   * We immediately invoke `db.file.Sync()` (which translates to `fsync(2)` on Linux/macOS and `FlushFileBuffers` on Windows). This system call blocks the CPU until the underlying hardware controller commits dirty blocks to non-volatile media.
   * `ACK` is dispatched to the client **strictly after** `Sync()` completes without error.
   * **The Invariant Holds:** If the process dies before `Sync()` returns, the write was unacknowledged (loss is permissible, truncation cleans it up). If it dies after `Sync()` returns, the bits are physically frozen on disk, and CRC verification on restart guarantees durability.

2. **Compaction Ordering:**
   $$\text{Write}(tmp) \longrightarrow tmp.\text{Sync}() \longrightarrow tmp.\text{Close}() \longrightarrow \text{Rename}(tmp, db) \longrightarrow \text{fsyncDir}(dir)$$
   * We write the compacted state to an independent sibling file (`path + ".tmp"`).
   * We call `Sync()` on the temporary file *before* attempting the swap.
   * We invoke `os.Rename(tmp, db)`, which leverages atomic directory pointer updates (`renameat` on POSIX, `SetFileInformationByHandle` on Windows NTFS).
   * We execute `fsyncDir()` on the parent directory handle. On POSIX filesystems, directory entry mutations require an explicit directory sync to guarantee that the pointer update survives a host power loss.
   * If a crash occurs at any point prior to rename, the old file is authoritative; if it occurs after rename, the new file is authoritative.

---

## 3. Known Weaknesses

We acknowledge the following three technical limitations in our design:

1. **Recovery is $O(N)$ with Total Operation Volume:**
   Because `KVdb` relies on an append-only log without periodic checkpoints, recovery scans the entire file sequentially from offset 8 to EOF. While our 1 MiB stream buffer processes $\approx 1,000,000$ records in $\approx 1.1\text{ s}$, an uncompacted database with $20,000,000$ historical mutations will exceed the 2-second recovery budget. Compaction must be scheduled periodically to bound recovery duration.

2. **Single Global Mutex Limits Concurrent Read Scalability:**
   To guarantee that concurrent readers never observe half-applied batches or dirty reads, `Get()` acquires `db.mu.Lock()`. While this eliminates race conditions and satisfies all Section 2 concurrency requirements, it forces readers to wait on long-running batch writes and disk syncs. A multi-version concurrency control (MVCC) or reader-writer lock (`sync.RWMutex`) with copy-on-write index snapshots would provide higher read throughput.

3. **Bit Rot in the Middle of Cold Records is Detected Only on Replay:**
   The CRC32 checksum is stored in the 8-byte frame header and verified during `recover()`. However, once the database is open, point lookups (`Get`) retrieve values directly from the in-memory RAM index. If a cold sector on disk suffers bit rot while the database is continuously online for months, the in-memory copy will remain intact until the next restart triggers a sequential scan.

---

## 4. What We Tried That Failed

1. **The Double-Descriptor File Lock Trap (`os.ReadFile` in `recover()`):**
   * *Attempt:* Originally, `recover()` read the database state using standard `os.ReadFile(db.path)`.
   * *Failure Found by Torcher:* On Windows testing, `NewDB` held an open `os.O_RDWR` descriptor on the file. Calling `os.ReadFile` opened a second descriptor without shared read flags, throwing intermittent `ERROR_SHARING_VIOLATION` panics during rapid crash-restart cycles.
   * *Resolution:* Abandoned `os.ReadFile` entirely. `recover()` now seeks to offset 0 on the existing `db.file` handle and streams via `bufio.NewReaderSize`.

2. **Truncation Without `Sync()` Exposed Old Tails:**
   * *Attempt:* During early development, torn tail healing called `db.file.Truncate(off)` and immediately returned without syncing.
   * *Failure Found by Torcher:* In multi-crash torture runs (Scenario F2), killing the process during restart immediately after `Truncate()` caused the filesystem metadata update to be lost. On the subsequent restart, the old torn bytes reappeared.
   * *Resolution:* Added mandatory `db.file.Sync()` immediately after `Truncate(off)` to freeze the truncation boundary before opening queries.

3. **Directory Fsync Permission Errors on Windows:**
   * *Attempt:* Strict directory `fsync` was initially executed via `os.Open(dir)` followed by `fd.Sync()`.
   * *Failure Found:* Windows returned `Access is denied` because Windows does not permit directory handle synchronization through generic POSIX abstractions.
   * *Resolution:* Implemented conditional error filtering: permission errors on directory handles are treated as benign on Windows, while preserving hard fail-stops on POSIX hosts.

---

## 5. If We Had 24 More Hours

1. **Write-Ahead Log (WAL) with Page-Based Storage:**
   Migrate from an append-only log to a fixed 4096-byte page storage layout with a circular WAL. This decouples recovery time from database size, enabling instantaneous crash recovery regardless of historical mutation volume.

2. **Lock-Free Read Path via Copy-on-Write (COW) Index:**
   Implement atomic pointer swaps on the in-memory radix tree. This allows concurrent `Get()` operations to run completely lock-free without blocking behind disk `fsync` operations.

3. **Background Incremental Compaction:**
   Replace the stop-the-world compaction routine with an asynchronous background compaction thread that processes segment generations without stalling write traffic.

