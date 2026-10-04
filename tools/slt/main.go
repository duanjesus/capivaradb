// Command slt runs sqllogictest scripts against CapivaraDB and reports how
// many records pass.
//
// sqllogictest is the test corpus SQLite uses to check itself against other
// databases: millions of queries with the results every engine is supposed
// to agree on. Running a slice of it measures compatibility with numbers
// instead of adjectives, and its failures are a to-do list.
//
// The script format is documented at
// https://www.sqlite.org/sqllogictest/doc/trunk/about.wiki. In short:
//
//	statement ok            a statement that must succeed
//	statement error         a statement that must fail
//	query III rowsort       a query, its column types and how to sort
//	SELECT ...
//	----
//	expected values, one per line, or "N values hashing to <md5>"
//
// The engine is driven in-process through the same Session interface the
// wire protocol uses.
package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duanjesus/capivaradb/internal/engine"
	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

// engineName is what "skipif" and "onlyif" directives are matched against.
// The scripts carry PostgreSQL-specific variants where dialects differ.
const engineName = "postgresql"

type record struct {
	line     int
	kind     string // "statement" or "query"
	wantErr  bool   // statement error
	types    string // query: one letter per column (I, R, T)
	sortMode string // query: nosort, rowsort, valuesort
	sql      string
	expected []string
}

// fileResult is the outcome of one script.
type fileResult struct {
	name    string
	total   int
	passed  int
	skipped int
	elapsed time.Duration
}

type runner struct {
	timeout  time.Duration
	verbose  int
	reasons  map[string]int
	examples map[string]string
	shown    int
}

func main() {
	root := flag.String("root", "", "directory the script paths are reported relative to")
	baseline := flag.String("baseline", "", "file with the pass counts to compare against")
	update := flag.Bool("update", false, "rewrite the baseline file with the current counts")
	timeout := flag.Duration("timeout", 5*time.Second, "time limit per record")
	verbose := flag.Int("v", 0, "print the first N failing records")
	markdown := flag.String("markdown", "", "write the report as a Markdown file")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: slt [flags] script.test ...")
		os.Exit(2)
	}

	r := &runner{timeout: *timeout, verbose: *verbose, reasons: map[string]int{}, examples: map[string]string{}}
	var results []fileResult
	for _, path := range flag.Args() {
		res, err := r.runFile(path, *root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "slt:", err)
			os.Exit(2)
		}
		results = append(results, res)
	}

	report := r.report(results)
	fmt.Print(report)
	if *markdown != "" {
		if err := os.WriteFile(*markdown, []byte(r.markdown(results)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "slt:", err)
			os.Exit(2)
		}
	}
	if *baseline == "" {
		return
	}
	if *update {
		var sb strings.Builder
		sb.WriteString("# Records passing per sqllogictest script. CI fails if a count drops.\n")
		sb.WriteString("# Regenerate with: bash scripts/slt.sh --update\n")
		for _, res := range results {
			fmt.Fprintf(&sb, "%s %d %d\n", res.name, res.passed, res.total)
		}
		if err := os.WriteFile(*baseline, []byte(sb.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "slt:", err)
			os.Exit(2)
		}
		fmt.Println("baseline updated:", *baseline)
		return
	}
	if regressions := compareBaseline(*baseline, results); len(regressions) > 0 {
		fmt.Println("\nREGRESSIONS against", *baseline)
		for _, line := range regressions {
			fmt.Println("  " + line)
		}
		os.Exit(1)
	}
	fmt.Println("\nno regressions against", *baseline)
}

func compareBaseline(path string, results []fileResult) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{err.Error()}
	}
	want := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && !strings.HasPrefix(line, "#") {
			want[fields[0]], _ = strconv.Atoi(fields[1])
		}
	}
	var out []string
	for _, res := range results {
		if base, ok := want[res.name]; ok && res.passed < base {
			out = append(out, fmt.Sprintf("%s: %d passed, baseline is %d", res.name, res.passed, base))
		}
	}
	return out
}

func (r *runner) report(results []fileResult) string {
	var sb strings.Builder
	var total, passed int
	fmt.Fprintf(&sb, "%-44s %8s %8s %7s %8s\n", "script", "records", "passed", "rate", "time")
	for _, res := range results {
		total += res.total
		passed += res.passed
		fmt.Fprintf(&sb, "%-44s %8d %8d %6.2f%% %7.1fs\n", res.name, res.total, res.passed, pct(res.passed, res.total), res.elapsed.Seconds())
	}
	fmt.Fprintf(&sb, "%-44s %8d %8d %6.2f%%\n", "TOTAL", total, passed, pct(passed, total))

	if len(r.reasons) > 0 {
		sb.WriteString("\nwhy records fail:\n")
		for _, reason := range r.sortedReasons() {
			fmt.Fprintf(&sb, "%8d  %s\n", r.reasons[reason], reason)
		}
	}
	return sb.String()
}

func (r *runner) markdown(results []fileResult) string {
	var sb strings.Builder
	var total, passed int
	sb.WriteString("| Script | Records | Passed | Rate |\n|---|---:|---:|---:|\n")
	for _, res := range results {
		total += res.total
		passed += res.passed
		fmt.Fprintf(&sb, "| `%s` | %d | %d | %.2f%% |\n", res.name, res.total, res.passed, pct(res.passed, res.total))
	}
	fmt.Fprintf(&sb, "| **Total** | **%d** | **%d** | **%.2f%%** |\n", total, passed, pct(passed, total))
	sb.WriteString("\n| Failures | Reason | Example |\n|---:|---|---|\n")
	for _, reason := range r.sortedReasons() {
		example := strings.ReplaceAll(r.examples[reason], "|", "\\|")
		fmt.Fprintf(&sb, "| %d | %s | `%s` |\n", r.reasons[reason], reason, example)
	}
	return sb.String()
}

func (r *runner) sortedReasons() []string {
	reasons := make([]string, 0, len(r.reasons))
	for reason := range r.reasons {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if r.reasons[reasons[i]] != r.reasons[reasons[j]] {
			return r.reasons[reasons[i]] > r.reasons[reasons[j]]
		}
		return reasons[i] < reasons[j]
	})
	return reasons
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// parseFile reads a script into records, applying skipif/onlyif and halt.
func parseFile(path string) (records []record, skipped int, threshold int, err error) {
	// Results longer than this many values are compared by hash. Scripts
	// may change it; the reference implementation starts at 8.
	threshold = 8
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	lineNo := 0
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		lineNo++
		return strings.TrimRight(sc.Text(), "\r"), true
	}
	// block reads lines up to a blank line or the given terminator.
	block := func(terminator string) (lines []string, terminated bool) {
		for {
			line, ok := next()
			if !ok || line == "" {
				return lines, false
			}
			if terminator != "" && line == terminator {
				return lines, true
			}
			lines = append(lines, line)
		}
	}

	skip := false
	for {
		line, ok := next()
		if !ok {
			break
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		switch fields[0] {
		case "skipif":
			skip = skip || (len(fields) > 1 && fields[1] == engineName)
		case "onlyif":
			skip = skip || (len(fields) > 1 && fields[1] != engineName)
		case "hash-threshold":
			if len(fields) > 1 {
				threshold, _ = strconv.Atoi(fields[1])
			}
		case "halt":
			if !skip {
				return records, skipped, threshold, sc.Err()
			}
			skip = false
		case "statement":
			rec := record{line: lineNo, kind: "statement", wantErr: len(fields) > 1 && fields[1] == "error"}
			lines, _ := block("")
			rec.sql = strings.Join(lines, "\n")
			if skip {
				skipped++
			} else {
				records = append(records, rec)
			}
			skip = false
		case "query":
			rec := record{line: lineNo, kind: "query", sortMode: "nosort"}
			if len(fields) > 1 {
				rec.types = fields[1]
			}
			if len(fields) > 2 {
				rec.sortMode = fields[2]
			}
			lines, more := block("----")
			rec.sql = strings.Join(lines, "\n")
			if more {
				rec.expected, _ = block("")
			}
			if skip {
				skipped++
			} else {
				records = append(records, rec)
			}
			skip = false
		default:
			return nil, 0, 0, fmt.Errorf("%s:%d: unknown record type %q", path, lineNo, fields[0])
		}
	}
	return records, skipped, threshold, sc.Err()
}

func (r *runner) runFile(path, root string) (fileResult, error) {
	records, skipped, threshold, err := parseFile(path)
	if err != nil {
		return fileResult{}, err
	}
	res := fileResult{name: filepath.ToSlash(path), total: len(records), skipped: skipped}
	if rel, err := filepath.Rel(root, path); root != "" && err == nil {
		res.name = filepath.ToSlash(rel)
	}

	// Every script gets a database of its own.
	db := engine.New()
	sess, err := db.NewSession(map[string]string{"user": "slt", "database": "slt"})
	if err != nil {
		return res, err
	}
	defer sess.Close()

	start := time.Now()
	for _, rec := range records {
		reason := r.runRecord(sess, rec, threshold)
		if reason == "" {
			res.passed++
			continue
		}
		r.reasons[reason]++
		if _, seen := r.examples[reason]; !seen {
			r.examples[reason] = oneLine(rec.sql, 110)
		}
		if r.shown < r.verbose {
			r.shown++
			fmt.Printf("FAIL %s:%d  %s\n     %s\n", res.name, rec.line, reason, oneLine(rec.sql, 300))
		}
	}
	res.elapsed = time.Since(start)

	// After tens of thousands of statements the storage must still be
	// intact: every B+tree valid, every index matching its table, no page
	// leaked. A violation is a bug, so it stops the run.
	sess.Close()
	if _, err := db.Verify(); err != nil {
		return res, fmt.Errorf("%s left the database inconsistent: %w", res.name, err)
	}
	return res, nil
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}

// runRecord executes one record and returns "" if it passed, or a short
// reason that failures can be grouped by.
func (r *runner) runRecord(sess pgwire.Session, rec record, threshold int) (reason string) {
	defer func() {
		// A panic is a bug in the engine, not a compatibility gap: make
		// it impossible to miss in the report.
		if p := recover(); p != nil {
			reason = fmt.Sprintf("PANIC: %v", p)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	cols, rows, err := execute(ctx, sess, rec.sql)
	if rec.kind == "statement" {
		switch {
		case rec.wantErr && err == nil:
			return "statement should have failed but succeeded"
		case !rec.wantErr && err != nil:
			return errorReason(err)
		}
		return ""
	}
	if err != nil {
		return errorReason(err)
	}
	if cols != len(rec.types) {
		return "wrong number of columns"
	}

	values := make([]string, 0, len(rows)*cols)
	for _, row := range rows {
		for i, v := range row {
			values = append(values, formatValue(v, rec.types[i]))
		}
	}
	switch rec.sortMode {
	case "rowsort":
		sortRows(values, cols)
	case "valuesort":
		sort.Strings(values)
	}

	if threshold > 0 && len(values) > threshold {
		h := md5.New()
		for _, v := range values {
			io.WriteString(h, v)
			io.WriteString(h, "\n")
		}
		values = []string{fmt.Sprintf("%d values hashing to %x", len(values), h.Sum(nil))}
	}
	if len(values) != len(rec.expected) {
		return "wrong result"
	}
	for i := range values {
		if values[i] != rec.expected[i] {
			return "wrong result"
		}
	}
	return ""
}

// sortRows sorts a flat list of values row by row, comparing rows column by
// column as strings, which is what the reference implementation does.
func sortRows(values []string, cols int) {
	if cols == 0 {
		return
	}
	n := len(values) / cols
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = append([]string(nil), values[i*cols:(i+1)*cols]...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for c := 0; c < cols; c++ {
			if rows[i][c] != rows[j][c] {
				return rows[i][c] < rows[j][c]
			}
		}
		return false
	})
	for i, row := range rows {
		copy(values[i*cols:], row)
	}
}

func execute(ctx context.Context, sess pgwire.Session, query string) (cols int, rows [][]any, err error) {
	defer func() {
		if err != nil {
			sess.OnError()
		}
	}()
	stmts, err := sess.Parse(query)
	if err != nil {
		return 0, nil, err
	}
	for _, st := range stmts {
		p, err := sess.Prepare(st, nil)
		if err != nil {
			return 0, nil, err
		}
		res, err := p.Execute(ctx, nil)
		if err != nil {
			return 0, nil, err
		}
		cols, rows = len(p.Columns()), nil
		for {
			row, err := res.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, nil, err
			}
			rows = append(rows, row)
		}
	}
	return cols, rows, nil
}

// formatValue renders a value the way the reference sqllogictest driver
// does for the column type the script declares.
func formatValue(v any, typ byte) string {
	if v == nil {
		return "NULL"
	}
	switch typ {
	case 'I':
		switch v := v.(type) {
		case int64:
			return strconv.FormatInt(v, 10)
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case bool:
			if v {
				return "1"
			}
			return "0"
		case string:
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return strconv.FormatInt(n, 10)
		}
	case 'R':
		switch v := v.(type) {
		case int64:
			return strconv.FormatFloat(float64(v), 'f', 3, 64)
		case float64:
			return strconv.FormatFloat(v, 'f', 3, 64)
		}
	}
	s := pgwire.TextValue(v)
	if s == "" {
		return "(empty)"
	}
	// Control and non-ASCII characters are replaced, so that results stay
	// comparable across encodings.
	b := []byte(s)
	for i, c := range b {
		if c < ' ' || c > '~' {
			b[i] = '@'
		}
	}
	return string(b)
}

// errorReason groups an error into a class: the SQLSTATE plus the message
// with the specifics (quoted names, numbers) removed.
func errorReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	pe := pgerr.From(err)
	if pe.Code == pgerr.QueryCanceled {
		return "timeout"
	}
	msg := pe.Message
	var sb strings.Builder
	inQuote := false
	for _, c := range msg {
		switch {
		case c == '"':
			if !inQuote {
				sb.WriteString(`"…"`)
			}
			inQuote = !inQuote
		case inQuote:
		case c >= '0' && c <= '9':
			if !strings.HasSuffix(sb.String(), "N") {
				sb.WriteByte('N')
			}
		default:
			sb.WriteRune(c)
		}
	}
	return pe.Code + " " + sb.String()
}
