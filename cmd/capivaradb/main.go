// Command capivaradb runs the database server.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"

	"github.com/duanjesus/capivaradb/internal/engine"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/storage"
	"github.com/duanjesus/capivaradb/internal/version"
)

func main() {
	// The default is loopback only: there is no authentication yet, so the
	// server must not be reachable from the network unless asked to.
	addr := flag.String("addr", "127.0.0.1:5432", "address to listen on")
	data := flag.String("data", "", "database file; without it the database lives in memory")
	cacheMB := flag.Int("cache", engine.DefaultPoolPages*storage.PageSize>>20, "buffer pool size in megabytes")
	check := flag.Bool("check", false, "verify the database file and exit")
	verbose := flag.Bool("v", false, "log every connection")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Full)
		return
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "capivaradb:", err)
		os.Exit(1)
	}

	db := engine.New()
	where := "in memory only: data is lost on exit"
	if *data != "" {
		var err error
		if db, err = engine.Open(*data, max(*cacheMB<<20/storage.PageSize, 8)); err != nil {
			fail(err)
		}
		where = "data file " + *data
	}

	if *check {
		report, err := db.Verify()
		if err != nil {
			fmt.Fprintln(os.Stderr, "capivaradb: the database is INCONSISTENT:", err)
			os.Exit(1)
		}
		fmt.Printf("ok: %d tables, %d indexes, %d rows; %d pages (%d kB), %d free\n",
			report.Tables, report.Indexes, report.Rows, report.Pages, report.Pages*storage.PageSize>>10, report.FreePages)
		db.Close()
		return
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(err)
	}
	srv := &pgwire.Server{Handler: db, Logger: logger}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		fmt.Fprintln(os.Stderr, "capivaradb: shutting down")
		srv.Close()
	}()

	fmt.Fprintf(os.Stderr, "%s\nlistening on %s (%s)\n", version.Full, ln.Addr(), where)
	serveErr := srv.Serve(ln)
	// Closing writes every modified page and syncs the file. Until the
	// write-ahead log exists this clean shutdown (or CHECKPOINT) is what
	// makes changes durable.
	if err := db.Close(); err != nil {
		fail(err)
	}
	if serveErr != nil {
		fail(serveErr)
	}
}
