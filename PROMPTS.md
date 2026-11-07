# AI Prompt Log

This file keeps the prompts used during the hackathon, in order. The original wording is preserved inside each entry. Times are approximate where noted; when the time was not recorded, it is marked that way rather than guessed.

## Draft Debugging Prompt

The following is a ready-to-use prompt about a specific issue spotted in `KVdb.go`. It is a draft, not a prompt already sent; after using it, add the actual interaction to the log below.

```
While reviewing our Go key-value database, I noticed that `Batch` silently treats a misspelled operation type as a put. For example, `Batch([]Op{{Type: "typo", Key: []byte("k"), Val: []byte("v")}})` currently succeeds and writes `k=v`, even though the caller supplied an invalid operation.

The issue appears in `KVdb.go`. In `encode`, the operation is assumed to be PUT unless its type is DEL:

    var t uint8 = 1
    if op.Type == "del" {
        t = 2
    }

There is a similar issue in `decode`: every serialized type other than 2 is treated as PUT:

    opType := "put"
    if t == 2 {
        opType = "del"
    }

Could you help us fix this without rewriting the file? Please make `encode` reject operation names other than "put" and "del", and make `decode` reject unknown type bytes. Add focused regression tests showing that an invalid batch returns an error without changing the database, and that malformed serialized data is rejected. Keep the existing file format and valid PUT/DEL behavior unchanged, and report the exact verification command and result.
```

## Prompts Used

---

### 1. Crash-safety audit
**Tool:** Antigravity / Claude Sonnet 4.6 (Thinking)  
**Time:** Hour 18 (2026-11-06 19:33 IST)  
**Prompt sent:**
```
ROLE: You are a principal systems architect and low-level storage engineer specializing in crash-safe embedded databases (like SQLite/RocksDB storage layers). You value hard facts, strict correctness under POSIX failure semantics, and zero fluff.

CONTEXT & PROBLEM GOAL:
We are participating in the "Kill It and Prove It" 24-hour engineering hackathon. We have built a single-process embedded key-value database in Go from scratch (KVdb.go, torcher.go, concurrent.go, STudentdb.go). The system must survive abrupt system shutdowns, power failures, or kill -9 process terminations at any arbitrary instruction. Once a write returns success (an ACK), the data must be fully persistent and survive computer restarts.

CRITICAL REQUIREMENTS & CONSTRAINTS:
1. OFFLINE EXECUTION: The database, testing tools, and debugging scripts must run 100% offline on a local laptop using only the Go standard library (os, sync, hash/crc32, encoding/binary, etc.). No external or cloud libraries are permitted.
2. ROM PERSISTENCE & METADATA: 
   - All authoritative state must live in a single file on disk (the ROM layer).
   - File-Level Metadata: The database initializes with an immutable 8-byte magic header (KVDB0001) written via atomic temp-file creation and directory fsync.
   - Record-Level Metadata: Every operation (put, del, batch) appends structured metadata including payload length, an IEEE CRC32 checksum, operation type, key length, and value length.
   - Crash Recovery (recover()): On startup after a computer shutdown or crash, the engine reads the file sequentially, verifies CRC32 checksums, and safely truncates any torn tails (half-written writes caused by an abrupt kill) back to the last valid state.
3. CLI COMMAND INTERFACE: Must support standard wrapper commands (open, put, get, del, batch, dump, check, serve).

YOUR OBJECTIVES FOR DEBUGGING & VERIFICATION:
- Audit the codebase for any race conditions, lock ordering flaws, or unhandled disk-full (ErrDiskFull) scenarios.
- Ensure that fsync calls (db.file.Sync() and fsyncDir) are properly executed before returning success ACKs to guarantee physical non-volatile disk persistence across system reboots.
- Verify that recovery completes in under 2 seconds for large datasets and handles torn tails flawlessly.

OUTPUT REQUIRED:
Provide the final audited code adjustments, verification steps, and a clean breakdown of how the ROM metadata is written, checked, and recovered to guarantee absolute survival across computer reboots.
```

---

### 2. Running the project
**Tool:** Antigravity / Claude Sonnet 4.6 (Thinking)  
**Time:** Hour 19 (2026-11-06 19:43 IST)  
**Prompt sent:**
```
how to run the project
```

---

### 3. Go command not found
**Tool:** Antigravity / Claude Sonnet 4.6 (Thinking)  
**Time:** Hour 19 (2026-11-06 19:59 IST)  
**Prompt sent:**
```
go : The term 'go' is not recognized as the name of a cmdlet, function, 
script file, or operable program. Check the spelling of the name, or if a 
path was included, verify that the path is correct and try again.
At line:1 char:1
+ go run KVdb.go put mydb.db hello world
+ ~~
    + CategoryInfo          : ObjectNotFound: (go:String) [], CommandNotFoun 
   dException
    + FullyQualifiedErrorId : CommandNotFoundException
```

---

### 4. Running in PowerShell
**Tool:** Antigravity / Claude Sonnet 4.6 (Thinking)  
**Time:** Hour 20 (2026-11-06 20:05 IST)  
**Prompt sent:**
```
i want to run thiis the powershell of antigravity
```

---

### 5. Add sample data
**Tool:** Antigravity / Claude Sonnet 4.6 (Thinking)  
**Time:** Hour 20 (2026-11-06 20:08 IST)  
**Prompt sent:**
```
add the all the data you can every where like 10 datas
```

---

### 6. Identify the metadata
**Tool:** Antigravity / Gemini 3.8 Flash  
**Time:** Hour 20 (2026-11-06 20:18 IST)  
**Prompt sent:**
```
which is the meta data in this
```

---

### 7. Create a metadata file
**Tool:** Antigravity / Gemini 3.8 Flash  
**Time:** Hour 20 (2026-11-06 20:21 IST)  
**Prompt sent:**
```
can create file for the metadata
```

---

### 8. Check the crash scenarios
**Tool:** Antigravity / Gemini 3.8 Flash  
**Time:** Hour 21 (2026-11-06 21:15 IST)  
**Prompt sent:**
```
ROLE: You are a principal systems architect specializing in crash-safe embedded key-value storage engines, transaction durability, and POSIX crash recovery semantics.

CONTEXT & PROBLEM GOAL:
We are building a single-process embedded key-value database in Go from scratch (KVdb.go, torcher.go, concurrent.go, STudentdb.go). The database stores all data in one file on disk (ROM/persistent layout) with an append-only log format, CRC32 checksums, and strict fsync durability boundaries.

ENVIRONMENT CONSTRAINTS:
- COMPLETELY OFFLINE: Use ONLY the standard Go library (os, sync, hash/crc32, encoding/binary, path/filepath, etc.).
- PERSISTENCE & METADATA: Every database file starts with an 8-byte magic header (KVDB0001) and uses structured record metadata (payload length, CRC32 checksum, op type, key/value lengths).
- CORRECTNESS INVARIANT: Once a write returns success (ACK), it must survive any process kill (kill -9). Unacknowledged writes may be lost, but must NEVER corrupt existing data, produce garbage values, or leave the file unrecoverable.

YOUR TASK:
Audit our codebase, identify any failure points, and verify that our implementation strictly satisfies all 32 crash scenarios across Groups A through F below:
[All 32 scenarios listed]
```

---



