package trace

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// bucketLabels are the concurrency buckets, in ascending order. A record's
// bucket is chosen by the total running at its start, itself included.
var bucketLabels = []string{"1", "2-3", "4-6", "7-10", "11+"}

func bucketIndex(total int) int {
	switch {
	case total <= 1:
		return 0
	case total <= 3:
		return 1
	case total <= 6:
		return 2
	case total <= 10:
		return 3
	}
	return 4
}

// Load reads every day file whose date is on or after since's local date
// and returns the records starting at or after since. Malformed lines are
// skipped; a missing directory is an empty log.
func Load(dir string, since time.Time) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	first := since.Format("2006-01-02")
	sinceEpoch := epoch(since)
	var out []Record
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || len(day) != len("2006-01-02") || day < first {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Kind == "" || r.Op == "" {
				continue
			}
			if r.At >= sinceEpoch {
				out = append(out, r)
			}
		}
		_ = f.Close()
	}
	return out, nil
}

// endHeap is a min-heap of end times.
type endHeap []float64

func (h endHeap) Len() int           { return len(h) }
func (h endHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h endHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *endHeap) Push(x any)        { *h = append(*h, x.(float64)) }
func (h *endHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Concurrency returns, for each record, how many OTHER non-lock records
// were running at its start (x.At <= r.At < x.End). Lock records are never
// counted. O(n log n): a sweep over start times with a heap of end times.
func Concurrency(recs []Record) []int {
	out := make([]int, len(recs))
	work := make([]int, 0, len(recs))
	for i, r := range recs {
		if r.Kind != KindLock {
			work = append(work, i)
		}
	}
	sort.Slice(work, func(a, b int) bool { return recs[work[a]].At < recs[work[b]].At })
	order := make([]int, len(recs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return recs[order[a]].At < recs[order[b]].At })

	var active endHeap
	next := 0
	for _, i := range order {
		r := recs[i]
		for next < len(work) && recs[work[next]].At <= r.At {
			heap.Push(&active, recs[work[next]].End)
			next++
		}
		for len(active) > 0 && active[0] <= r.At {
			heap.Pop(&active)
		}
		n := len(active)
		// A work record is in the heap itself unless it has no duration.
		if r.Kind != KindLock && r.End > r.At {
			n--
		}
		out[i] = n
	}
	return out
}

// percentile is the nearest-rank p-th percentile of sorted.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// Bucket is the records of one group at one concurrency level.
type Bucket struct {
	Count int   `json:"count"`
	P50   int64 `json:"p50_ms"`
}

// Group is one kind+op with its timing and per-concurrency breakdown.
type Group struct {
	Kind        string            `json:"kind"`
	Op          string            `json:"op"`
	Count       int               `json:"count"`
	P50         int64             `json:"p50_ms"`
	P95         int64             `json:"p95_ms"`
	Max         int64             `json:"max_ms"`
	Total       int64             `json:"total_ms"`
	Sensitivity *float64          `json:"sensitivity"`
	Buckets     map[string]Bucket `json:"buckets"`
}

// LockWait is one lock's wait statistics.
type LockWait struct {
	Op    string `json:"op"`
	Count int    `json:"count"`
	P50   int64  `json:"p50_ms"`
	P95   int64  `json:"p95_ms"`
	Max   int64  `json:"max_ms"`
	Total int64  `json:"total_ms"`
}

// Report is the analysis of a window of records.
type Report struct {
	Since   int64      `json:"since"`
	Records int        `json:"records"`
	Groups  []Group    `json:"groups"`
	Locks   []LockWait `json:"locks"`
}

// Options narrow what a report lists. Concurrency is always measured over
// every record, so filtering never hides load from the numbers.
type Options struct {
	Kinds    []string
	OpPrefix string
	MinCount int
}

func (o Options) keep(r Record) bool {
	if o.OpPrefix != "" && !strings.HasPrefix(r.Op, o.OpPrefix) {
		return false
	}
	if len(o.Kinds) == 0 {
		return true
	}
	for _, k := range o.Kinds {
		if k == r.Kind {
			return true
		}
	}
	return false
}

func sortedMS(recs []Record, idx []int) []int64 {
	ms := make([]int64, len(idx))
	for i, j := range idx {
		ms[i] = recs[j].MS
	}
	sort.Slice(ms, func(a, b int) bool { return ms[a] < ms[b] })
	return ms
}

// Analyze groups recs by kind+op and measures how each group's duration
// moves with the concurrency at its start.
func Analyze(recs []Record, since time.Time, o Options) Report {
	conc := Concurrency(recs)
	rep := Report{Since: since.Unix(), Records: len(recs), Groups: []Group{}, Locks: []LockWait{}}

	type key struct{ kind, op string }
	groups := map[key][]int{}
	for i, r := range recs {
		if o.keep(r) {
			k := key{r.Kind, r.Op}
			groups[k] = append(groups[k], i)
		}
	}
	for k, idx := range groups {
		ms := sortedMS(recs, idx)
		var total int64
		for _, v := range ms {
			total += v
		}
		if k.kind == KindLock {
			rep.Locks = append(rep.Locks, LockWait{
				Op: k.op, Count: len(ms), P50: percentile(ms, 50), P95: percentile(ms, 95),
				Max: ms[len(ms)-1], Total: total,
			})
			continue
		}
		g := Group{
			Kind: k.kind, Op: k.op, Count: len(ms),
			P50: percentile(ms, 50), P95: percentile(ms, 95), Max: ms[len(ms)-1], Total: total,
			Buckets: map[string]Bucket{},
		}
		per := make([][]int, len(bucketLabels))
		for _, j := range idx {
			b := bucketIndex(conc[j] + 1)
			per[b] = append(per[b], j)
		}
		lo, hi := -1, -1
		var loP50, hiP50 int64
		for b, members := range per {
			if len(members) == 0 {
				continue
			}
			p50 := percentile(sortedMS(recs, members), 50)
			g.Buckets[bucketLabels[b]] = Bucket{Count: len(members), P50: p50}
			if len(members) >= o.MinCount {
				if lo < 0 {
					lo, loP50 = b, p50
				}
				hi, hiP50 = b, p50
			}
		}
		if lo >= 0 && hi != lo && loP50 > 0 {
			s := float64(hiP50) / float64(loP50)
			g.Sensitivity = &s
		}
		rep.Groups = append(rep.Groups, g)
	}
	sort.Slice(rep.Groups, func(a, b int) bool {
		x, y := rep.Groups[a], rep.Groups[b]
		if (x.Sensitivity == nil) != (y.Sensitivity == nil) {
			return x.Sensitivity != nil
		}
		if x.Sensitivity != nil && *x.Sensitivity != *y.Sensitivity {
			return *x.Sensitivity > *y.Sensitivity
		}
		if x.Total != y.Total {
			return x.Total > y.Total
		}
		return x.Kind+" "+x.Op < y.Kind+" "+y.Op
	})
	sort.Slice(rep.Locks, func(a, b int) bool {
		if rep.Locks[a].Total != rep.Locks[b].Total {
			return rep.Locks[a].Total > rep.Locks[b].Total
		}
		return rep.Locks[a].Op < rep.Locks[b].Op
	})
	return rep
}

// FormatMS prints a duration for people: 850ms, 12.3s, 4m05s.
func FormatMS(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	s := ms / 1000
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// WriteText renders rep as the human-readable report.
func WriteText(w io.Writer, rep Report) {
	fmt.Fprintf(w, "trace since %s: %d records\n",
		time.Unix(rep.Since, 0).Format("2006-01-02 15:04"), rep.Records)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nOP\tN\tP50\tP95\tMAX\tP50 BY CONCURRENCY (N)\tSENSITIVITY")
	for _, g := range rep.Groups {
		var parts []string
		for _, l := range bucketLabels {
			if b, ok := g.Buckets[l]; ok {
				parts = append(parts, fmt.Sprintf("%s:%s(%d)", l, FormatMS(b.P50), b.Count))
			}
		}
		sens := "-"
		if g.Sensitivity != nil {
			sens = fmt.Sprintf("×%.1f", *g.Sensitivity)
		}
		fmt.Fprintf(tw, "%s %s\t%d\t%s\t%s\t%s\t%s\t%s\n", g.Kind, g.Op, g.Count,
			FormatMS(g.P50), FormatMS(g.P95), FormatMS(g.Max), strings.Join(parts, " "), sens)
	}
	_ = tw.Flush()
	if len(rep.Locks) == 0 {
		return
	}
	fmt.Fprintln(w, "\nlock waits")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LOCK\tN\tP50\tP95\tMAX\tTOTAL")
	for _, l := range rep.Locks {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", l.Op, l.Count,
			FormatMS(l.P50), FormatMS(l.P95), FormatMS(l.Max), FormatMS(l.Total))
	}
	_ = tw.Flush()
}
