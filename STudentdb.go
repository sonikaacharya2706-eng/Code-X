package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const dbPath = "students.db"

type Student struct {
	Name   string
	Branch string
	Year   string
}

func loadStudents(db *DB) map[string]Student {
	db.mu.Lock()
	defer db.mu.Unlock()

	out := make(map[string]Student)
	var keys []string
	for k := range db.index {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		parts := strings.Split(k, ":")
		if len(parts) >= 3 && parts[0] == "student" {
			sid := parts[1]
			field := parts[2]
			s := out[sid]
			val := string(db.index[k])
			switch field {
			case "name":
				s.Name = val
			case "branch":
				s.Branch = val
			case "year":
				s.Year = val
			}
			out[sid] = s
		}
	}
	return out
}

func showStudent(sid string, s Student) {
	fmt.Printf("--- student %s ---\n", sid)
	fmt.Printf("name   %s\n", s.Name)
	fmt.Printf("branch %s\n", s.Branch)
	fmt.Printf("year   %s\n", s.Year)
}

func addStudent(db *DB, name, branch, year string) {
	val := db.Get([]byte("meta:next_id"))
	n := 1
	if val != nil {
		if parsed, err := strconv.Atoi(string(val)); err == nil {
			n = parsed
		}
	}
	sid := fmt.Sprintf("%04d", n)

	ops := []Op{
		{Type: "put", Key: []byte(fmt.Sprintf("student:%s:name", sid)), Val: []byte(name)},
		{Type: "put", Key: []byte(fmt.Sprintf("student:%s:branch", sid)), Val: []byte(branch)},
		{Type: "put", Key: []byte(fmt.Sprintf("student:%s:year", sid)), Val: []byte(year)},
		{Type: "put", Key: []byte("meta:next_id"), Val: []byte(strconv.Itoa(n + 1))},
	}

	if err := db.Batch(ops); err != nil {
		fmt.Fprintf(os.Stderr, "Error adding student: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("added student", sid)
}

func printUsage() {
	fmt.Println(`Student records stored in kvdb.

Each student becomes 3 keys that share an id:
    student:0001:name   -> gaurav
    student:0001:branch -> ise
    student:0001:year   -> 2026-2028
A counter key (meta:next_id) hands out ids. All 4 keys are written in ONE batch,
so a student is saved completely or not at all, even if the program is killed.

Usage:
    go run students.go main.go add "Gaurav" ise 2026-2028
    go run students.go main.go list
    go run students.go main.go find ise          (search by name, branch or year)
    go run students.go main.go delete 0001`)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		return
	}

	db, err := NewDB(dbPath, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	cmd := args[0]
	switch {
	case cmd == "add" && len(args) == 4:
		addStudent(db, args[1], args[2], args[3])

	case cmd == "list":
		data := loadStudents(db)
		if len(data) == 0 {
			fmt.Println("no students yet")
			return
		}
		var sids []string
		for sid := range data {
			sids = append(sids, sid)
		}
		sort.Strings(sids)
		for _, sid := range sids {
			showStudent(sid, data[sid])
		}

	case cmd == "find" && len(args) == 2:
		word := strings.ToLower(args[1])
		data := loadStudents(db)
		var hits []string
		for sid, s := range data {
			if strings.Contains(strings.ToLower(s.Name), word) ||
				strings.Contains(strings.ToLower(s.Branch), word) ||
				strings.Contains(strings.ToLower(s.Year), word) {
				hits = append(hits, sid)
			}
		}
		if len(hits) == 0 {
			fmt.Println("no match")
			return
		}
		sort.Strings(hits)
		for _, sid := range hits {
			showStudent(sid, data[sid])
		}

	case cmd == "delete" && len(args) == 2:
		sid := args[1]
		data := loadStudents(db)
		if _, exists := data[sid]; !exists {
			fmt.Println("no such student", sid)
			return
		}
		ops := []Op{
			{Type: "del", Key: []byte(fmt.Sprintf("student:%s:name", sid)), Val: nil},
			{Type: "del", Key: []byte(fmt.Sprintf("student:%s:branch", sid)), Val: nil},
			{Type: "del", Key: []byte(fmt.Sprintf("student:%s:year", sid)), Val: nil},
		}
		if err := db.Batch(ops); err != nil {
			fmt.Fprintf(os.Stderr, "Error deleting student: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("deleted student", sid)

	default:
		printUsage()
	}
}