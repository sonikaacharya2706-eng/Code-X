# KVdb Metadata Specification & ROM Layout

This document describes the on-disk format, binary framing, integrity mechanisms, and recovery semantics used in `KVdb`.

---

## 1. Overview of Metadata Layers

The database defines two tiers of metadata:
1. **Engine / ROM Metadata (On-Disk):** Structural byte framing that ensures integrity, crash safety, and torn-tail detection across hard kills (`kill -9`, power failure).
2. **Application Metadata (In-Database):** Logical key-space markers used by user programs (e.g., auto-increment sequences like `meta:next_id`).

---

## 2. On-Disk Binary Layout

All authoritative data lives in a single append-only log file.

```
+-----------------------------------------------------------------------------------+
| FILE HEADER (8 bytes, immutable)                                                  |
| Offset 0x00 - 0x07: "KVDB0001" (Magic ASCII identifier)                           |
+-----------------------------------------------------------------------------------+
| RECORD 1                                                                          |
|   +-------------------------------------------------------------------------------+
|   | Frame Header (8 bytes)                                                        |
|   |   Offset 0x08 - 0x0B: payload_len (uint32, 4 bytes, Little-Endian)             |
|   |   Offset 0x0C - 0x0F: crc32       (uint32, 4 bytes, IEEE 802.3 Checksum)      |
|   +-------------------------------------------------------------------------------+
|   | Payload (payload_len bytes)                                                   |
|   |   +---------------------------------------------------------------------------+
|   |   | op_count (uint32, 4 bytes, Little-Endian)                                 |
|   |   +---------------------------------------------------------------------------+
|   |   | Operation 1:                                                              |
|   |   |   type    : uint8  (1 byte  -> 0x01 = PUT, 0x02 = DEL)                    |
|   |   |   key_len : uint16 (2 bytes -> Little-Endian)                             |
|   |   |   val_len : uint32 (4 bytes -> Little-Endian)                             |
|   |   |   key     : [key_len]bytes                                                |
|   |   |   val     : [val_len]bytes                                                |
|   |   +---------------------------------------------------------------------------+
|   |   | Operation 2 ... N                                                         |
+-----------------------------------------------------------------------------------+
| RECORD 2 ... M (Appended sequentially)                                            |
+-----------------------------------------------------------------------------------+
```

---

## 3. Field Specification

| Component | Field | Type | Size | Endianness | Description |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **File Header** | `MAGIC` | `[]byte` | 8 bytes | ASCII | `KVDB0001`. Must match on startup or recovery aborts. |
| **Record Frame** | `payload_len` | `uint32` | 4 bytes | Little-Endian | Total byte size of the payload following the frame header. |
| | `crc32` | `uint32` | 4 bytes | Little-Endian | IEEE CRC-32 computed strictly over the payload bytes. |
| **Payload Envelope** | `op_count` | `uint32` | 4 bytes | Little-Endian | Number of discrete operations contained within the transaction. |
| **Per Operation** | `type` | `uint8` | 1 byte | Native | `0x01` = `put`, `0x02` = `del` (tombstone). |
| | `key_len` | `uint16` | 2 bytes | Little-Endian | Length of the key slice ($1 \le \text{len} \le 1024$). |
| | `val_len` | `uint32` | 4 bytes | Little-Endian | Length of value slice ($0 \le \text{len} \le 1048576$). 0 for `del`. |
| | `key` | `[]byte` | Variable | Raw bytes | Key identifier. |
| | `val` | `[]byte` | Variable | Raw bytes | Value contents. |

---

## 4. Live Record Breakdown (`data.db`)

Real hex trace from `data.db` after inserting `city=Bangalore` inside a 10-key batch:

```hex
Offset      Hex Bytes                                         ASCII / Interpretation
-----------------------------------------------------------------------------------------
00000000    4B 56 44 42 30 30 30 31                           Magic Header ("KVDB0001")
00000008    C6 00 00 00                                       payload_len = 198 bytes (0x000000C6)
0000000C    98 9C 90 B4                                       CRC32 = 0xB4909C98 (IEEE checksum)
00000010    0A 00 00 00                                       op_count = 10 operations
00000014    01                                                op[0].type = 1 (PUT)
00000015    04 00                                             op[0].key_len = 4 bytes
00000017    09 00 00 00                                       op[0].val_len = 9 bytes
0000001B    63 69 74 79                                       op[0].key = "city"
0000001F    42 61 6E 67 61 6C 6F 72 65                        op[0].val = "Bangalore"
... (remaining 9 operations continue sequentially)
```

---

## 5. Recovery & Torn Tail Truncation Algorithm

When recovering after an abrupt shutdown or `kill -9`:

```
                       [Start: seek(0)]
                              │
                    [Read 8-byte Magic]
                              │
                    Does Magic == "KVDB0001"?
                     ├──────────────┴──────────────┐
                    YES                            NO
                     │                             │
             [off = 8 bytes]                 [Fail Hard]
                     │
      ┌──────────────┴───────────────────────────┐
      ▼                                          │
[Read 8-byte frame header (len, crc)]            │
      │                                          │
Reached clean EOF?                               │
      ├───────────────────────┐                  │
     NO                      YES                 │
      │                       │                  │
Is (off + 8 + len) > file_size?                  │
      ├───────────────────────┤                  │
     NO                      YES (Torn length)   │
      │                       │                  │
[Read payload bytes]          │                  │
      │                       │                  │
Computed CRC32 == stored CRC? │                  │
      ├───────────────────────┤                  │
     YES                     NO (Torn/Corrupt)   │
      │                       │                  │
[Decode ops & apply to map]   │                  │
[off += 8 + len]              │                  │
      │                       │                  │
      └───────────────────────┼──────────────────┘
                              ▼
                 Did loop stop before EOF?
                     ├─────────────────┴─────────────────┐
                    YES                                 NO
                     │                                   │
         [file.Truncate(off)]                 [DB recovery ready]
         [file.Sync()]                                   ▲
                     │                                   │
                     └───────────────────────────────────┘
```

1. **Length Validation:** If `off + 8 + len > file_size`, the process died while writing payload bytes. The tail is discarded.
2. **Checksum Integrity:** If the CRC32 check fails, sectors were corrupted or partially flushed. The engine halts scanning and rolls back.
3. **Truncation & Durability:** `file.Truncate(off)` followed by `file.Sync()` guarantees that rollback is durable before answering queries.

---

## 6. Write Ordering & Atomicity Guarantees

Every mutation (`put`, `del`, `batch`) follows this sequence:

1. **`encode(ops)`**: Binary serialization and CRC calculation in user RAM buffer.
2. **`mu.Lock()`**: Serialization lock preventing interleaved writes.
3. **`WriteAt(rec, size)`**: Data written into OS page cache.
4. **`file.Sync()`**: POSIX `fsync` / Windows `FlushFileBuffers` forces cache to disk.
5. **`apply(ops)`**: In-memory hash index updated.
6. **`size += len(rec)`**: Internal file size marker updated.
7. **`mu.Unlock()`**: Lock released.
8. **`ACK` output**: Acknowledgment sent to client.

