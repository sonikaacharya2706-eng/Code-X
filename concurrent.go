package main

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func childMain(path string) {
	db, err := NewDB(path, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening DB in child: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	var outMu sync.Mutex

	writer := func(wid int) {
		i := 0
		for {
			i++
			v := []byte(fmt.Sprintf("val-%d-%d", wid, i))
			k1 := []byte(fmt.Sprintf("w%d-%d", wid, i))
			k2 := []byte("shared")

			// D1: this writer's own key, D2: both writers fight over this key
			if err := db.Put(k1, v); err != nil {
				return
			}
			if err := db.Put(k2, v); err != nil {
				return
			}

			outMu.Lock()
			fmt.Printf("ACK %d %d\n", wid, i)
			outMu.Unlock()
		}
	}

	go writer(0)
	go writer(1)

	// Keep alive until killed
	select {}
}

func oneRound(rnd *rand.Rand, path string) (bool, string) {
	acked := map[int]int{0: 0, 1: 0}
	var ackedMu sync.Mutex

	executable, err := os.Executable()
	if err != nil {
		// Fallback to go run or current binary if os.Executable is tricky
		executable = "go"
	}

	var cmd *exec.Cmd
	if executable == "go" {
		cmd = exec.Command("go", "run", "main.go", "concurrent_test.go", "--child", path)
	} else {
		cmd = exec.Command(executable, "--child", path)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Sprintf("failed to get stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return false, fmt.Sprintf("failed to start child: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "ACK") {
				parts := strings.Split(line, " ")
				if len(parts) >= 3 {
					w, err1 := strconv.Atoi(parts[1])
					n, err2 := strconv.Atoi(parts[2])
					if err1 == nil && err2 == nil {
						ackedMu.Lock()
						if n > acked[w] {
							acked[w] = n
						}
						ackedMu.Unlock()
					}
				}
			}
		}
	}()

	// Random kill moment between 0.2 and 0.7 seconds
	sleepMs := 200 + rnd.Intn(500)
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	wg.Wait()

	db, err := NewDB(path, nil)
	if err != nil {
		return false, fmt.Sprintf("failed to reopen db: %v", err)
	}
	defer db.Close()

	db.mu.Lock()
	defer db.mu.Unlock()

	// D1: every acked key present with the right value
	for w := 0; w <= 1; w++ {
		for n := 1; n <= acked[w]; n++ {
			k := fmt.Sprintf("w%d-%d", w, n)
			got := db.index[k]
			expected := fmt.Sprintf("val-%d-%d", w, n)
			if got == nil || string(got) != expected {
				var gotStr string
				if got != nil {
					gotStr = string(got)
				}
				return false, fmt.Sprintf("D1 FAIL: acked w%d-%n found %q", w, n, gotStr)
			}
		}
	}

	// No wrong values anywhere in w keys
	for k, v := range db.index {
		if strings.HasPrefix(k, "w") {
			parts := strings.Split(k[1:], "-")
			if len(parts) == 2 {
				wStr, nStr := parts[0], parts[1]
				expected := fmt.Sprintf("val-%s-%s", wStr, nStr)
				if string(v) != expected {
					return false, fmt.Sprintf("D1 FAIL: key %s holds wrong value %q", k, string(v))
				}
			}
		}
	}

	// D2: shared must be a value that was really sent (n at most acked+1 per writer)
	shared := db.index["shared"]
	if shared == nil {
		if acked[0]+acked[1] > 0 {
			return false, "D2 FAIL: shared missing though puts were ACKed"
		}
	} else {
		sharedStr := string(shared)
		parts := strings.Split(sharedStr, "-")
		if len(parts) == 3 && parts[0] == "val" {
			w, err1 := strconv.Atoi(parts[1])
			n, err2 := strconv.Atoi(parts[2])
			ackedMu.Lock()
			maxAck := acked[w]
			ackedMu.Unlock()
			if err1 != nil || err2 != nil || n > maxAck+1 {
				return false, fmt.Sprintf("D2 FAIL: shared holds %q, never written properly", sharedStr)
			}
		} else {
			return false, fmt.Sprintf("D2 FAIL: shared holds invalid format %q", sharedStr)
		}
	}

	return true, fmt.Sprintf("ok (writer0 acked=%d, writer1 acked=%d)", acked[0], acked[1])
}

func main() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--child" && i+1 < len(args) {
			childMain(args[i+1])
			return
		}
	}

	iters := 30
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
	d, err := os.MkdirTemp("", "concurrent_test_*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(d)

	for it := 0; it < iters; it++ {
		dbPath := filepath.Join(d, fmt.Sprintf("t%d.db", it))
		ok, msg := oneRound(rnd, dbPath)
		fmt.Printf("iter %d: %s\n", it, msg)
		if !ok {
			fmt.Printf("FAIL, stopping. (seed=%d, iteration=%d)\n", seed, it)
			os.Exit(1)
		}
	}
	fmt.Printf("PASS %d/%d\n", iters, iters)
}