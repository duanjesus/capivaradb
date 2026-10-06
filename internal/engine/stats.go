package engine

import (
	"math"

	"github.com/duanjesus/capivaradb/internal/sql"
)

// Table statistics are what the planner knows about the data without
// looking at it: how many rows a table has, how many distinct values a
// column has, what range they span. They are gathered by ANALYZE, which
// also runs by itself once a table has changed enough, and are stored in
// the catalog so that a restart does not make the planner blind.
//
// They are estimates of a past state, deliberately. A plan does not have
// to be based on exact numbers, only on numbers good enough to tell a
// lookup from a scan of a million rows.

type tableStats struct {
	rows float64
	cols []colStats
}

type colStats struct {
	// distinct is the number of distinct non-NULL values.
	distinct float64
	// nullFrac is the fraction of rows in which the column is NULL.
	nullFrac float64
	// min and max bound the values of a numeric column; hasRange says
	// whether they are set.
	hasRange bool
	min, max float64
}

// autoAnalyzeMin is the least number of row changes that triggers an
// automatic ANALYZE; a table must also have changed by a fifth.
const autoAnalyzeMin = 50

// planStats is the planner's view of a table, copied out under the
// database lock so that planning can go on without it.
type planStats struct {
	rows    float64
	stats   *tableStats // nil if the table has never been analysed
	indexes []*index
}

// forPlanning captures what the planner needs to know about t. The caller
// must hold db.mu.
func (t *table) forPlanning() planStats {
	rows := float64(t.delta)
	if t.stats != nil {
		rows += t.stats.rows
	}
	// Never zero: an estimate of zero rows would make every plan on top
	// look free, and the table may have grown since it was last counted.
	return planStats{rows: math.Max(rows, 1), stats: t.stats, indexes: t.indexes}
}

// distinct estimates the number of distinct values of column col.
func (ps planStats) distinct(t *table, col int) float64 {
	if ps.stats != nil && col < len(ps.stats.cols) && ps.stats.cols[col].distinct > 0 {
		// The table may have grown since; distinct values cannot
		// outnumber rows, but can have kept pace with them.
		return math.Min(ps.stats.cols[col].distinct, ps.rows)
	}
	if t.uniqueOn(col, ps.indexes) {
		return ps.rows
	}
	// Nothing is known. Assuming a value repeats about twice is a guess
	// that makes an equality selective without making it unique.
	return math.Max(ps.rows/2, 1)
}

// uniqueOn reports whether column col alone is declared unique.
func (t *table) uniqueOn(col int, indexes []*index) bool {
	if len(t.pk) == 1 && t.pk[0] == col {
		return true
	}
	for _, ix := range indexes {
		if ix.unique && len(ix.cols) == 1 && ix.cols[0] == col {
			return true
		}
	}
	return false
}

// analyze computes the statistics of t from the rows the snapshot sees. The
// caller must hold db.mu.
func (db *DB) analyze(t *table, sn *snapshot) (*tableStats, error) {
	rows, err := db.scan(t, sn)
	if err != nil {
		return nil, err
	}
	st := &tableStats{rows: float64(len(rows)), cols: make([]colStats, len(t.cols))}
	for c, col := range t.cols {
		seen := make(map[string]struct{})
		nulls := 0
		cs := &st.cols[c]
		var key []byte
		for _, r := range rows {
			v := r.vals[c]
			if v == nil {
				nulls++
				continue
			}
			key = appendKey(key[:0], v)
			seen[string(key)] = struct{}{}
			if col.typ.IsNumeric() {
				f := toFloat(v)
				if !cs.hasRange {
					cs.hasRange, cs.min, cs.max = true, f, f
				}
				cs.min, cs.max = math.Min(cs.min, f), math.Max(cs.max, f)
			}
		}
		cs.distinct = float64(len(seen))
		if len(rows) > 0 {
			cs.nullFrac = float64(nulls) / float64(len(rows))
		}
	}
	return st, nil
}

// storeStats makes st the statistics of t, in memory and in the catalog.
func (db *DB) storeStats(t *table, st *tableStats, ch *changes) error {
	var w recWriter
	w.float(st.rows)
	w.uint(uint64(len(st.cols)))
	for _, c := range st.cols {
		w.float(c.distinct)
		w.float(c.nullFrac)
		w.bool(c.hasRange)
		w.float(c.min)
		w.float(c.max)
	}
	key := catalogKey('s', t.name)
	old, found, err := db.catalog.Get(key)
	if err != nil {
		return err
	}
	if found {
		err = ch.replace(db.catalog, key, old, w.b)
	} else {
		err = ch.put(db.catalog, key, w.b)
	}
	if err != nil {
		return err
	}
	prev, prevDelta, prevMods := t.stats, t.delta, t.mods
	t.stats, t.delta, t.mods = st, 0, 0
	ch.onUndo(func() { t.stats, t.delta, t.mods = prev, prevDelta, prevMods })
	return nil
}

func decodeStats(rec []byte, ncols int) *tableStats {
	r := recReader{b: rec}
	st := &tableStats{rows: r.float()}
	n := r.uint()
	if r.bad || n != uint64(ncols) {
		return nil
	}
	st.cols = make([]colStats, n)
	for i := range st.cols {
		st.cols[i] = colStats{distinct: r.float(), nullFrac: r.float(), hasRange: r.bool(), min: r.float(), max: r.float()}
	}
	if r.bad {
		return nil
	}
	return st
}

// needsAnalyze reports whether enough of t has changed since its statistics
// were gathered for them to be worth gathering again.
func (t *table) needsAnalyze() bool {
	rows := 0.0
	if t.stats != nil {
		rows = t.stats.rows
	}
	return float64(t.mods) >= math.Max(autoAnalyzeMin, rows/5)
}

// Selectivities used when nothing better is known. They are PostgreSQL's.
const (
	defaultSel      = 1.0 / 3
	defaultRangeSel = 1.0 / 3
	defaultLikeSel  = 0.1
)

// rangeSelectivity estimates the fraction of a numeric column's values that
// satisfy "col op bound", by assuming they are spread evenly between the
// smallest and the largest.
func (cs colStats) rangeSelectivity(op string, bound float64) float64 {
	if !cs.hasRange || cs.max <= cs.min {
		return defaultRangeSel
	}
	below := (bound - cs.min) / (cs.max - cs.min)
	below = math.Min(math.Max(below, 0), 1)
	if op == "<" || op == "<=" {
		return math.Max(below, 0.001)
	}
	return math.Max(1-below, 0.001)
}

// typeFamily groups types whose values encode alike in a key.
func sameKeyFamily(col, val sql.Type) bool {
	switch {
	case col.IsInt():
		return val.IsInt()
	case col == sql.Float8:
		return val.IsNumeric()
	}
	return col == val
}
