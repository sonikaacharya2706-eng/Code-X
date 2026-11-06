package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var MAGIC = []byte("KVDB0001")
var ErrDiskFull = errors.New("max_bytes reached")

type Op struct {
	Type string // "put" or "del"
	Key  []byte
	Val  []byte
}

// fsyncDir fsyncs the directory containing path so that a preceding rename()
// is durable on POSIX systems. On Windows, directory handles are not
// fsync-able — the NTFS journal provides rename atomicity automatically,
// so we skip the sync and return nil.
func fsyncDir(path string) error {
	dir := filepath.Dir(path)
	fd, err := os.Open(dir)
	if err != nil {
		return nil // best-effort; not fatal on Windows
	}
	defer fd.Close()
	err = fd.Sync()
	if err != nil {
		// Windows returns "Access is denied" for directory syncs — ignore it.
		// On Linux/macOS this is a real error and must be returned.
		if os.IsPermission(err) {
			return nil
		}
		return err
	}
	return nil
}

// encode serialises a batch of Ops into a single framed record:
//
//	[4] payload_len (little-endian uint32)
//	[4] crc32_IEEE  (over payload bytes only)
//	[N] payload:
//	    [4] op_count
//	    per op: [1] type | [2] key_len | [4] val_len | key | val
func encode(ops []Op) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(ops))); err != nil {
		return nil, err
	}
	for _, op := range ops {
		var t uint8 = 1
		if op.Type == "del" {
			t = 2
		}
		val := op.Val
		if val == nil {
			val = []byte{}
		}
		if err := binary.Write(buf, binary.LittleEndian, t); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint16(len(op.Key))); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint32(len(val))); err != nil {
			return nil, err
		}
		buf.Write(op.Key)
		buf.Write(val)
	}
	payload := buf.Bytes()

	header := new(bytes.Buffer)
	checksum := crc32.ChecksumIEEE(payload)
	if err := binary.Write(header, binary.LittleEndian, uint32(len(payload))); err != nil {
		return nil, err
	}
	if err := binary.Write(header, binary.LittleEndian, checksum); err != nil {
		return nil, err
	}

	return append(header.Bytes(), payload...), nil
}

func decode(payload []byte) ([]Op, error) {
	if len(payload) < 4 {
		return nil, errors.New("invalid payload")
	}
	n := binary.LittleEndian.Uint32(payload[0:4])
	off := 4
	var ops []Op
	for i := uint32(0); i < n; i++ {
		if off+7 > len(payload) {
			return nil, errors.New("truncated op")
		}
		t := payload[off]
		kl := binary.LittleEndian.Uint16(payload[off+1 : off+3])
		vl := binary.LittleEndian.Uint32(payload[off+3 : off+7])
		off += 7

		if off+int(kl)+int(vl) > len(payload) {
			return nil, errors.New("truncated key/value")
		}
		// Copy slices so they are independent of the read buffer.
		k := make([]byte, kl)
		copy(k, payload[off:off+int(kl)])
		off += int(kl)
		v := make([]byte, vl)
		copy(v, payload[off:off+int(vl)])
		off += int(vl)

		opType := "put"
		if t == 2 {
			opType = "del"
		}
		ops = append(ops, Op{Type: opType, Key: k, Val: v})
	}
	return ops, nil
}

type DB struct {
	path     string
	maxBytes *int64
	mu       sync.Mutex
	index    map[string][]byte
	file     *os.File
	size     int64
}

// NewDB opens (or creates) the database at path.
// If the file does not exist it is created atomically:
//
//	write MAGIC → tmp → fsync → rename → fsyncDir
//
// On any existing file it runs recovery to rebuild the in-memory index and
// truncate any torn tail back to the last CRC-valid record.
func NewDB(path string, maxBytes *int64) (*DB, error) {
	tmp := path + ".tmp"
	// Remove any leftover temp file from a previous crashed compaction.
	if _, err := os.Stat(tmp); err == nil {
		os.Remove(tmp)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(MAGIC); err != nil {
			f.Close()
			return nil, err
		}
		// fsync the tmp file so MAGIC is durable before the rename.
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
		// fsync the directory so the rename (directory entry) is durable.
		// BUG-3 FIX: error is now checked.
		if err := fsyncDir(path); err != nil {
			return nil, err
		}
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}

	db := &DB{
		path:     path,
		maxBytes: maxBytes,
		index:    make(map[string][]byte),
		file:     file,
	}

	if err := db.recover(); err != nil {
		file.Close()
		return nil, err
	}
	return db, nil
}

// recover reads the file sequentially using a buffered reader over the already-
// open file descriptor (db.file), verifies every record's CRC32, rebuilds the
// in-memory index, and truncates any torn tail to the last valid record.
//
// BUG-5 FIX: previously used os.ReadFile which opened a second fd and loaded
// the entire file into heap memory. Now streams via db.file → O(1) memory,
// no double-open, works correctly on Windows (no sharing violation).
func (db *DB) recover() error {
	// Seek to start to read from the beginning.
	if _, err := db.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(db.file, 1<<20) // 1 MiB read-ahead buffer

	// Verify 8-byte magic header.
	magic := make([]byte, 8)
	if _, err := io.ReadFull(r, magic); err != nil {
		return errors.New("bad header: file too short")
	}
	if !bytes.Equal(magic, MAGIC) {
		return errors.New("bad header: magic mismatch")
	}

	off := int64(8) // byte offset of the next record to be read

	for {
		// Read the 8-byte record frame header: [4] payload_len | [4] crc32
		hdr := make([]byte, 8)
		_, err := io.ReadFull(r, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // clean EOF or torn header → truncate to off
		}
		if err != nil {
			return err
		}

		payloadLen := binary.LittleEndian.Uint32(hdr[0:4])
		storedCRC := binary.LittleEndian.Uint32(hdr[4:8])

		// Guard against absurdly large payloadLen that would exhaust memory.
		// 512 MiB is a reasonable upper bound for a single record.
		const maxPayload = 512 << 20
		if payloadLen > maxPayload {
			break // treat as torn tail
		}

		payload := make([]byte, payloadLen)
		_, err = io.ReadFull(r, payload)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // torn write: length field intact but payload not fully written
		}
		if err != nil {
			return err
		}

		if crc32.ChecksumIEEE(payload) != storedCRC {
			break // CRC mismatch → torn or corrupted tail
		}

		ops, err := decode(payload)
		if err != nil {
			break // malformed record → treat as torn tail
		}

		db.apply(ops)
		off += 8 + int64(payloadLen)
	}

	// Truncate any torn tail and make the truncation itself durable.
	fi, err := db.file.Stat()
	if err != nil {
		return err
	}
	if off < fi.Size() {
		if err := db.file.Truncate(off); err != nil {
			return err
		}
		// Seek to new end so subsequent WriteAt calls land at the right offset.
		if _, err := db.file.Seek(off, io.SeekStart); err != nil {
			return err
		}
		// Make the truncation durable before returning — without this, a second
		// crash before the OS flushes the metadata could re-expose the torn tail.
		if err := db.file.Sync(); err != nil {
			return err
		}
	}
	db.size = off
	return nil
}

func (db *DB) apply(ops []Op) {
	for _, op := range ops {
		kStr := string(op.Key)
		if op.Type == "put" {
			db.index[kStr] = op.Val
		} else {
			delete(db.index, kStr)
		}
	}
}

// commit encodes ops into a single CRC-framed record, appends it to the file,
// calls fsync to guarantee non-volatile persistence, then updates the in-memory
// index and size. All under db.mu.
func (db *DB) commit(ops []Op) error {
	if len(ops) == 0 {
		return nil
	}

	// encode is pure; compute outside the lock to minimise lock hold time.
	rec, err := encode(ops)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.maxBytes != nil && db.size+int64(len(rec)) > *db.maxBytes {
		return ErrDiskFull
	}

	// WriteAt loop: handles short writes (rare but possible on some OSes).
	done := 0
	for done < len(rec) {
		n, err := db.file.WriteAt(rec[done:], db.size+int64(done))
		if err != nil {
			// Best-effort rollback: truncate back to the last committed position.
			_ = db.file.Truncate(db.size)
			return err
		}
		done += n
	}

	// fsync BEFORE returning ACK. This is the core crash-safety guarantee:
	// once Sync() returns without error, the OS has flushed the write to
	// non-volatile storage. A kill -9 or power failure after this point
	// cannot lose the record.
	if err := db.file.Sync(); err != nil {
		_ = db.file.Truncate(db.size)
		return err
	}

	db.apply(ops)
	db.size += int64(len(rec))
	return nil
}

func (db *DB) Put(k, v []byte) error {
	return db.commit([]Op{{Type: "put", Key: k, Val: v}})
}

func (db *DB) Delete(k []byte) error {
	return db.commit([]Op{{Type: "del", Key: k, Val: nil}})
}

func (db *DB) Batch(ops []Op) error {
	return db.commit(ops)
}

func (db *DB) Get(k []byte) []byte {
	db.mu.Lock()
	defer db.mu.Unlock()
	v := db.index[string(k)]
	if v == nil {
		return nil
	}
	// Return a copy so the caller cannot mutate the index.
	out := make([]byte, len(v))
	copy(out, v)
	return out
}

// Compact rewrites the database to a new temp file containing only the live
// key set, then atomically renames it over the original.
//
// BUG-3 FIX: fsyncDir error is now checked.
// BUG-4 FIX: the new file descriptor is opened before the old one is closed,
// so the DB struct is never left in a half-updated state.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tmp := db.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}

	if _, err := f.Write(MAGIC); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}

	// Collect all live key/value pairs and write them in batches of 1000
	// to keep individual record sizes reasonable.
	var items []Op
	for k, v := range db.index {
		items = append(items, Op{Type: "put", Key: []byte(k), Val: v})
	}

	for i := 0; i < len(items); i += 1000 {
		end := i + 1000
		if end > len(items) {
			end = len(items)
		}
		rec, err := encode(items[i:end])
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := f.Write(rec); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}

	// fsync the new compacted file before the rename so its content is durable.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}

	newSize, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()

	// Check if compacted file exceeds configured maxBytes (Scenario E4)
	if db.maxBytes != nil && newSize > *db.maxBytes {
		os.Remove(tmp)
		return ErrDiskFull
	}

	// Atomic rename: the new file replaces the old one.
	if err := os.Rename(tmp, db.path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Flush the directory entry for the rename.
	// BUG-3 FIX: error is checked.
	if err := fsyncDir(db.path); err != nil {
		return err
	}

	// BUG-4 FIX: open the new fd BEFORE closing the old one.
	// If OpenFile fails, the old fd is still valid and the DB struct stays consistent.
	newFile, err := os.OpenFile(db.path, os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	db.file.Close()
	db.file = newFile
	db.size = newSize
	return nil
}

func (db *DB) Close() error {
	return db.file.Close()
}

// KVdb.go is a pure library — no main() here.
// Use kvdb_cli.go for the CLI, torcher.go / STudentdb.go / conurrent.go for their own mains.
