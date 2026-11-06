package main

// kvdb_cli.go — CLI entry point matching the exact hackathon command specification:
//
//   ./kvdb open <path> [--max-bytes N] # opens or creates, runs recovery, prints key count and recovery time
//   ./kvdb put <path> <key> <value>
//   ./kvdb get <path> <key>            # prints value, or NOTFOUND
//   ./kvdb del <path> <key>
//   ./kvdb batch <path> <file>         # file has one op per line: "put k v" or "del k"
//   ./kvdb dump <path>                 # prints every key and value, sorted by key
//   ./kvdb check <path>                # verifies file integrity, prints OK or a reason
//   ./kvdb serve <path>                # long-running mode: reads ops from stdin, prints ACK after each success

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func serve(path string, maxBytes *int64) {
	db, err := NewDB(path, maxBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	scanner := bufio.NewScanner(os.Stdin)
	const maxTokenSize = 2 * 1024 * 1024
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxTokenSize)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 2 {
			continue
		}
		id := parts[0]
		cmd := parts[1]

		var opErr error
		switch cmd {
		case "put":
			if len(parts) >= 4 {
				opErr = db.Put([]byte(parts[2]), []byte(parts[3]))
			}
		case "del":
			if len(parts) >= 3 {
				opErr = db.Delete([]byte(parts[2]))
			}
		case "compact":
			opErr = db.Compact()
		}

		if errors.Is(opErr, ErrDiskFull) {
			fmt.Printf("ERR %s diskfull\n", id)
		} else if opErr != nil {
			fmt.Printf("ERR %s %v\n", id, opErr)
		} else {
			fmt.Printf("ACK %s\n", id)
		}
	}
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Println("Usage:")
		fmt.Println("  kvdb open <path> [--max-bytes N]")
		fmt.Println("  kvdb put <path> <key> <value>")
		fmt.Println("  kvdb get <path> <key>")
		fmt.Println("  kvdb del <path> <key>")
		fmt.Println("  kvdb batch <path> <file>")
		fmt.Println("  kvdb dump <path>")
		fmt.Println("  kvdb check <path>")
		fmt.Println("  kvdb serve <path> [--max-bytes N]")
		os.Exit(1)
	}

	cmd := args[0]
	path := args[1]

	var maxBytes *int64
	for i := 2; i < len(args); i++ {
		if args[i] == "--max-bytes" && i+1 < len(args) {
			var mb int64
			fmt.Sscanf(args[i+1], "%d", &mb)
			maxBytes = &mb
			break
		}
	}

	if cmd == "serve" {
		serve(path, maxBytes)
		return
	}

	start := time.Now()
	db, err := NewDB(path, maxBytes)
	recoveryTime := time.Since(start)

	if err != nil {
		if cmd == "check" {
			fmt.Printf("CORRUPTED: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	switch cmd {
	case "open":
		db.mu.Lock()
		count := len(db.index)
		db.mu.Unlock()
		fmt.Printf("OK %d keys, recovery time: %v\n", count, recoveryTime)

	case "put":
		if len(args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: kvdb put <path> <key> <value>")
			os.Exit(1)
		}
		if err := db.Put([]byte(args[2]), []byte(args[3])); err != nil {
			fmt.Fprintf(os.Stderr, "put error: %v\n", err)
			os.Exit(1)
		}

	case "get":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: kvdb get <path> <key>")
			os.Exit(1)
		}
		v := db.Get([]byte(args[2]))
		if v == nil {
			fmt.Println("NOTFOUND")
		} else {
			fmt.Println(string(v))
		}

	case "del":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: kvdb del <path> <key>")
			os.Exit(1)
		}
		if err := db.Delete([]byte(args[2])); err != nil {
			fmt.Fprintf(os.Stderr, "del error: %v\n", err)
			os.Exit(1)
		}

	case "batch":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: kvdb batch <path> <file>")
			os.Exit(1)
		}
		batchFile := args[2]
		fileData, err := os.ReadFile(batchFile)
		var ops []Op
		if err == nil {
			// Read operations from file: one per line ("put k v" or "del k")
			// Set scanner buffer to 2 MB to handle 1 MB values without bufio.ErrTooLong
			scanner := bufio.NewScanner(strings.NewReader(string(fileData)))
			const maxTokenSize = 2 * 1024 * 1024
			buf := make([]byte, 64*1024)
			scanner.Buffer(buf, maxTokenSize)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, " ", 3)
				switch parts[0] {
				case "put":
					if len(parts) == 3 {
						ops = append(ops, Op{Type: "put", Key: []byte(parts[1]), Val: []byte(parts[2])})
					}
				case "del":
					if len(parts) >= 2 {
						ops = append(ops, Op{Type: "del", Key: []byte(parts[1]), Val: nil})
					}
				}
			}
		} else {
			// Fallback: parse direct inline key=val pairs
			for _, arg := range args[2:] {
				kv := strings.SplitN(arg, "=", 2)
				if len(kv) == 2 {
					ops = append(ops, Op{Type: "put", Key: []byte(kv[0]), Val: []byte(kv[1])})
				}
			}
		}

		if len(ops) == 0 {
			fmt.Fprintln(os.Stderr, "No valid operations found in batch")
			os.Exit(1)
		}
		if err := db.Batch(ops); err != nil {
			fmt.Fprintf(os.Stderr, "batch error: %v\n", err)
			os.Exit(1)
		}

	case "dump":
		db.mu.Lock()
		var keys []string
		for k := range db.index {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s %s\n", k, string(db.index[k]))
		}
		db.mu.Unlock()

	case "check":
		db.mu.Lock()
		count := len(db.index)
		db.mu.Unlock()
		fmt.Printf("OK %d keys\n", count)
	}
}
