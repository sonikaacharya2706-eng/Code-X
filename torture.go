package main

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var keys = []string{}

func init() {
	for i := 0; i < 30; i++ {
		keys = append(keys, fmt.Sprintf("k%d", i))
	}
}

// torchOp is an internal tracking struct for the torture harness.
// It is distinct from the DB's Op type (defined in KVdb.go).
type torchOp struct {
	key string
	val *string // nil for del
}

func oneRound(rnd *rand.Rand, path string, state map[string]string, counter *uint64) (bool, string) {
	ops := make(map[uint64]torchOp)
	perKey := make(map[string][]uint64)
	lastAck := make(map[string]int64)

	cmd := exec.Command("go", "run", "KVdb.go", "serve", path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, fmt.Sprintf("failed to get stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Sprintf("failed to get stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return false, fmt.Sprintf("failed to start cmd: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	// Reader goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "ACK") {
				parts := strings.Split(line, " ")
				if len(parts) >= 2 {
					id, err := strconv.ParseUint(parts[1], 10, 64)
					if err == nil {
						mu.Lock()
						op, exists := ops[id]
						if exists {
							k := op.key
							if cur, ok := lastAck[k]; !ok || int64(id) > cur {
								lastAck[k] = int64(id)
							}
						}
						mu.Unlock()
					}
				}
			}
		}
	}()

	// Writer goroutine
	stopWriter := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		rndW := rand.New(rand.NewSource(rnd.Int63()))
		for {
			select {
			case <-stopWriter:
				return
			default:
			}

			i := atomic.AddUint64(counter, 1) - 1
			k := keys[rndW.Intn(len(keys))]

			mu.Lock()
			var line string
			var op torchOp
			if rndW.Float64() < 0.25 {
				op = torchOp{key: k, val: nil}
				line = fmt.Sprintf("%d del %s\n", i, k)
			} else {
				vChars := make([]byte, rndW.Intn(201))
				for j := range vChars {
					vChars[j] = 'x'
				}
				v := fmt.Sprintf("v%d-%s", i, string(vChars))
				op = torchOp{key: k, val: &v}
				line = fmt.Sprintf("%d put %s %s\n", i, k, v)
			}
			ops[i] = op
			perKey[k] = append(perKey[k], i)
			mu.Unlock()

			if _, err := stdin.Write([]byte(line)); err != nil {
				return
			}
		}
	}()

	// Random sleep time between 0.15 and 0.6 seconds
	sleepMs := 150 + rnd.Intn(450)
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	// Kill process abruptly (SIGKILL equivalent)
	close(stopWriter)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	_ = stdin.Close()
	wg.Wait()

	// Reopen DB and verify
	db, err := NewDB(path, nil)
	if err != nil {
		return false, fmt.Sprintf("failed to reopen db: %v", err)
	}
	defer db.Close()

	mu.Lock()
	defer mu.Unlock()

	for _, k := range keys {
		gotBytes := db.Get([]byte(k))
		var got *string
		if gotBytes != nil {
			s := string(gotBytes)
			got = &s
		}

		var allowed []*string
		if ackId, ok := lastAck[k]; ok {
			for _, id := range perKey[k] {
				if int64(id) >= ackId {
					allowed = append(allowed, ops[id].val)
				}
			}
		} else {
			stateVal, hasState := state[k]
			var ptr *string
			if hasState {
				ptr = &stateVal
			}
			allowed = append(allowed, ptr)
			for _, id := range perKey[k] {
				allowed = append(allowed, ops[id].val)
			}
		}

		found := false
		for _, a := range allowed {
			if (got == nil && a == nil) || (got != nil && a != nil && *got == *a) {
				found = true
				break
			}
		}

		if !found {
			allowedStrs := []string{}
			for _, a := range allowed {
				if a == nil {
					allowedStrs = append(allowedStrs, "None")
				} else {
					if len(*a) > 20 {
						allowedStrs = append(allowedStrs, (*a)[:20]+"...")
					} else {
						allowedStrs = append(allowedStrs, *a)
					}
				}
			}
			gotStr := "None"
			if got != nil {
				if len(*got) > 20 {
					gotStr = (*got)[:20] + "..."
				} else {
					gotStr = *got
				}
			}
			lastAckVal := int64(-1)
			if v, ok := lastAck[k]; ok {
				lastAckVal = v
			}
			return false, fmt.Sprintf("key %s: found %s, allowed %v, last_ack=%d", k, gotStr, allowedStrs, lastAckVal)
		}
	}

	db.mu.Lock()
	var extra []string
	for k := range db.index {
		found := false
		for _, validK := range keys {
			if k == validK {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, k)
		}
	}
	db.mu.Unlock()

	if len(extra) > 0 {
		sort.Strings(extra)
		return false, fmt.Sprintf("unknown keys %v", extra)
	}

	db.mu.Lock()
	for k := range state {
		delete(state, k)
	}
	for k, v := range db.index {
		state[k] = string(v)
	}
	db.mu.Unlock()

	nAcked := len(lastAck)
	return true, fmt.Sprintf("ok (acked-keys=%d)", nAcked)
}

func main() {
	args := os.Args[1:]
	iters := 100
	seed := 1

	for i := 0; i < len(args); i++ {
		if args[i] == "--iterations" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				iters = n
			}
		}
		if args[i] == "--seed" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				seed = n
			}
		}
	}

	rnd := rand.New(rand.NewSource(int64(seed)))
	d, err := os.MkdirTemp("", "kvdb_torture_*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(d)

	path := filepath.Join(d, "t.db")
	state := make(map[string]string)
	passed := 0
	var counter uint64

	for it := 0; it < iters; it++ {
		ok, msg := oneRound(rnd, path, state, &counter)
		fmt.Printf("iter %d: %s\n", it, msg)
		if !ok {
			fmt.Printf("FAIL, stopping. (seed=%d, iteration=%d)\n", seed, it)
			os.Exit(1)
		}
		passed++
	}
	fmt.Printf("PASS %d/%d\n", passed, iters)
}