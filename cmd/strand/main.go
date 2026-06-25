// Command strand serves a human-friendly web UI over a beads (bd) workspace.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/server"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/web"
)

// Version is stamped at build time via -ldflags '-X main.Version=...' (see Makefile).
var Version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	dir := flag.String("dir", "", "seed this beads workspace into the registry and make it active")
	bin := flag.String("bd", "bd", "path to the bd binary")
	northStar := flag.String("northstar", "", "north-star line shown above the strand")
	flag.Parse()

	tmpl, err := web.Templates()
	if err != nil {
		log.Fatalf("strand: parse templates: %v", err)
	}

	reg, err := registry.Open(registry.ConfigPath(), registry.ScanRoot())
	if err != nil {
		log.Fatalf("strand: open registry: %v", err)
	}
	// -dir seeds and activates an explicit workspace (handy for a repo outside
	// the ~/Projects scan); a current-directory .beads is the bare-launch default.
	if seed := seedDir(*dir); seed != "" {
		if _, err := reg.Add(seed); err != nil {
			log.Printf("strand: seed %s: %v", seed, err)
		}
	}

	// srcFor scopes a fresh bd client to the active repo's path, so switching the
	// active repo re-scopes every read and write (spec D3).
	bdBin := *bin
	srcFor := func(repo registry.Repo) server.IssueSource {
		return &bd.Client{Dir: repo.Path, Bin: bdBin}
	}
	syn := strand.Synthesis{NorthStar: *northStar}
	srv := server.New(srcFor, reg, tmpl, web.Static(), syn)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Shut down cleanly on Ctrl-C / kill so the user gets a goodbye instead of a
	// silent drop, and in-flight requests get a moment to finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind before announcing, so a failed bind reports the conflict instead of a
	// misleading "serving" line followed by an error.
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", *addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			log.Fatalf("strand: looks like a strand is already running on %s — no need to start another. 🌲\n"+
				"  to use that one:    open http://%s\n"+
				"  to restart fresh:   pkill -f 'strand --addr'  &&  strand\n"+
				"  to run side by side: strand --addr 127.0.0.1:7778", *addr, *addr)
		}
		log.Fatalf("strand: listen %s: %v", *addr, err)
	}

	log.Printf("strand %s: serving http://%s  (ctrl-c to stop)", Version, *addr)
	if err := serve(ctx, httpSrv, ln); err != nil {
		log.Fatalf("strand: %v", err)
	}
}

// serve runs httpSrv on ln until ctx is cancelled, then drains in-flight
// requests before returning. The shutdownDone handshake is the point: Serve
// unblocks at the START of Shutdown, so without waiting on it the caller would
// return mid-drain and drop the very in-flight requests graceful shutdown is
// meant to protect. Returns nil on a clean shutdown; a non-nil error only for a
// genuine Serve failure (e.g. the listener dies).
func serve(ctx context.Context, httpSrv *http.Server, ln net.Listener) error {
	shutdownDone := make(chan struct{})
	serveDone := make(chan struct{})
	// Detached from ctx on purpose: by the time this drains, ctx is already
	// cancelled, so the grace period must come from a fresh context.
	go func() { //nolint:gosec // G118: the drain's shutdown context is deliberately detached from the cancelled ctx
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			log.Printf("strand: shutting down — bye")
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(shutCtx); err != nil {
				log.Printf("strand: shutdown: %v", err)
			}
		case <-serveDone:
			// Serve failed before any shutdown — exit instead of leaking here.
		}
	}()

	err := httpSrv.Serve(ln)
	close(serveDone)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	// Serve returned ErrServerClosed — a shutdown is in progress. Wait for the
	// goroutine to finish draining before returning, so in-flight requests land.
	<-shutdownDone
	return nil
}

// seedDir resolves the workspace to seed at launch: the -dir flag if set, else
// the current directory when it holds a .beads workspace, else nothing (the user
// picks a repo from the selector). registry.Add validates the .beads itself.
func seedDir(dir string) string {
	if dir != "" {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(wd, ".beads")); err != nil {
		return ""
	}
	return wd
}
