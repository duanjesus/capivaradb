package engine

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKillProcess is the blunt version of the crash tests: a real process
// writes to a real database file and is killed — SIGKILL, or
// TerminateProcess on Windows — at a random moment, over and over. After
// each kill the parent opens the file, which runs recovery, and checks that
// every transaction the child reported as committed is there, whole, and
// that nothing else is.
//
// The child is this same test binary, started again with an environment
// variable that sends it down the writer's path.
//
// Killing a process loses what was in its memory (the buffer pool, the
// unwritten log) but not what the operating system had already been handed,
// so this does not test fsync placement; TestCrashRecovery does that, on a
// simulated disk. What this adds is the real thing end to end: real files,
// real process death, real recovery.
const killEnv = "CAPIVARA_KILL_TEST_DB"

// Each transaction number n inserts rowsPerTx rows into the ledger and adds
// to two running totals. The totals make atomicity checkable: they must
// always agree with the ledger.
const rowsPerTx = 40

func killChild(path string) {
	// A small pool so that pages of the transaction in progress are
	// written to the data file before it commits: after a kill those are
	// what recovery has to undo. A small checkpoint threshold so that
	// kills land in checkpoints too.
	db, err := Open(path, Options{PoolPages: 12, CheckpointBytes: 512 << 10})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	sess, _ := db.NewSession(map[string]string{"user": "child"})
	run := func(q string) {
		if err := execSQL(sess, q); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
	}
	run(`create table if not exists ledger (id int primary key, tx int not null, payload text);
	     create table if not exists totals (k int primary key, txs int not null, amount bigint not null);
	     create index if not exists ledger_tx on ledger (tx)`)

	// Carry on from wherever the last incarnation got to.
	h := &harness{sess: sess}
	last, _, err := h.run("select coalesce(max(tx), 0) from ledger")
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	n, _ := strconv.Atoi(last)
	if n == 0 {
		run("insert into totals values (1, 0, 0)")
	}
	payload := strings.Repeat("capivara ", 60)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		n++
		run("begin")
		for i := 0; i < rowsPerTx; i++ {
			run(fmt.Sprintf("insert into ledger values (%d, %d, '%s%d')", n*rowsPerTx+i, n, payload, i))
		}
		run(fmt.Sprintf("update totals set txs = txs + 1, amount = amount + %d where k = 1", n))
		// Now and then give up instead, which must leave no trace.
		if rng.Intn(5) == 0 {
			run("rollback")
			n--
			continue
		}
		run("commit")
		// Only now may the parent count on it.
		fmt.Println("committed", n)
	}
}

func TestKillProcess(t *testing.T) {
	if path := os.Getenv(killEnv); path != "" {
		killChild(path)
		return
	}
	iterations := 12
	if testing.Short() {
		iterations = 4
	}
	path := filepath.Join(t.TempDir(), "kill.cdb")
	rng := rand.New(rand.NewSource(1))
	acked := 0
	var rolledBack, redone int

	for i := 0; i < iterations; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKillProcess$")
		cmd.Env = append(os.Environ(), killEnv+"="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Let it run for a while, then kill it without warning.
		lines := make(chan string)
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		kill := time.After(time.Duration(100+rng.Intn(500)) * time.Millisecond)
		killed := false
		for line := range lines {
			if strings.HasPrefix(line, "error:") {
				t.Fatalf("iteration %d: the writer failed: %s", i, line)
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "committed ")); err == nil {
				acked = n
			}
			select {
			case <-kill:
				if !killed {
					cmd.Process.Kill()
					killed = true
				}
			default:
			}
		}
		if !killed {
			cmd.Process.Kill()
		}
		cmd.Wait()

		// Recovery happens here, when the file is opened.
		db, err := Open(path, Options{PoolPages: 256})
		if err != nil {
			t.Fatalf("iteration %d: recovery failed: %v", i, err)
		}
		info := db.Recovery()
		rolledBack += info.RolledBack
		redone += info.PagesRedone
		if _, err := db.Verify(); err != nil {
			t.Fatalf("iteration %d: the recovered database is inconsistent: %v", i, err)
		}
		h := newHarness(t, db)
		got := h.mustRun("select coalesce(max(tx), 0), count(*), count(distinct tx) from ledger")
		var maxTx, rows, txs int
		fmt.Sscanf(strings.ReplaceAll(got, "|", " "), "%d %d %d", &maxTx, &rows, &txs)

		// Durability: nothing acknowledged may be missing. One more
		// transaction than acknowledged is fine: it committed, and the
		// process died before it could say so.
		if maxTx < acked || maxTx > acked+1 {
			t.Fatalf("iteration %d: %d transactions were acknowledged, the database has %d", i, acked, maxTx)
		}
		// Atomicity: every transaction is there in full, and the totals,
		// updated in the same transactions, agree with the ledger.
		if rows != txs*rowsPerTx {
			t.Fatalf("iteration %d: %d rows for %d transactions: some transaction is there in part", i, rows, txs)
		}
		h.expect("select txs from totals", fmt.Sprint(txs))
		h.expect("select amount from totals", h.mustRun("select coalesce(sum(tx), 0) / "+fmt.Sprint(rowsPerTx)+" from ledger"))
		h.sess.Close()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		acked = maxTx
	}
	if acked == 0 {
		t.Fatal("the writer never committed anything")
	}
	t.Logf("%d kills survived; %d transactions committed in all, none lost; recovery replayed %d page changes and rolled back %d unfinished transactions",
		iterations, acked, redone, rolledBack)
}
