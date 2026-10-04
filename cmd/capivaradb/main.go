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
	"github.com/duanjesus/capivaradb/internal/version"
)

func main() {
	// The default is loopback only: there is no authentication yet, so the
	// server must not be reachable from the network unless asked to.
	addr := flag.String("addr", "127.0.0.1:5432", "address to listen on")
	verbose := flag.Bool("v", false, "log every connection")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Full)
		return
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capivaradb:", err)
		os.Exit(1)
	}
	srv := &pgwire.Server{Handler: engine.New(), Logger: logger}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		fmt.Fprintln(os.Stderr, "capivaradb: shutting down")
		srv.Close()
	}()

	fmt.Fprintf(os.Stderr, "%s\nlistening on %s (data is in memory only and is lost on exit)\n", version.Full, ln.Addr())
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "capivaradb:", err)
		os.Exit(1)
	}
}
