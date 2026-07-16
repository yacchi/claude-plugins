package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Task struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Priority string `json:"priority"`
	DueDate  string `json:"dueDate"`
	Status   string `json:"status"`
}

var (
	mu      sync.Mutex
	tasks   = []Task{}
	nextID  = 1
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	prioMap = map[string]int{"low": 0, "medium": 1, "high": 2}
)

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func isValidDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		Priority string `json:"priority"`
		DueDate  string `json:"dueDate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeErr(w, http.StatusBadRequest, "title must not be empty")
		return
	}
	if _, ok := prioMap[body.Priority]; !ok {
		writeErr(w, http.StatusBadRequest, "priority must be one of low, medium, high")
		return
	}
	if !isValidDate(body.DueDate) {
		writeErr(w, http.StatusBadRequest, "dueDate must be YYYY-MM-DD")
		return
	}

	mu.Lock()
	id := fmt.Sprintf("task-%d", nextID)
	nextID++
	t := Task{ID: id, Title: title, Priority: body.Priority, DueDate: body.DueDate, Status: "pending"}
	tasks = append(tasks, t)
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(t)
}

func handleList(w http.ResponseWriter, r *http.Request) {
	sortBy := r.URL.Query().Get("sort")
	order := r.URL.Query().Get("order")
	if order == "" {
		order = "asc"
	}

	mu.Lock()
	out := make([]Task, len(tasks))
	copy(out, tasks)
	mu.Unlock()

	less := func(i, j int) bool {
		var primaryLess, primaryEqual bool
		switch sortBy {
		case "priority":
			pi, pj := prioMap[out[i].Priority], prioMap[out[j].Priority]
			primaryLess = pi < pj
			primaryEqual = pi == pj
		default: // "dueDate" or unspecified: default sort is by dueDate
			primaryLess = out[i].DueDate < out[j].DueDate
			primaryEqual = out[i].DueDate == out[j].DueDate
		}
		if primaryEqual {
			// tie-break: always dueDate ascending regardless of primary sort field
			return out[i].DueDate < out[j].DueDate
		}
		if order == "desc" {
			return !primaryLess
		}
		return primaryLess
	}
	sort.SliceStable(out, less)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handlePatch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Status != "pending" && body.Status != "done" {
		writeErr(w, http.StatusBadRequest, "status must be pending or done")
		return
	}

	mu.Lock()
	defer mu.Unlock()
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].Status = body.Status
			json.NewEncoder(w).Encode(tasks[i])
			return
		}
	}
	writeErr(w, http.StatusNotFound, "task not found")
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleCreate(w, r)
		case http.MethodGet:
			handleList(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/tasks/")
		if r.Method != http.MethodPatch || id == "" {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handlePatch(w, r, id)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("listening on :" + port)
	http.ListenAndServe(":"+strconv.Itoa(mustAtoi(port)), mux)
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
