// Command strand serves a human-friendly web UI over a beads (bd) workspace.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
	"github.com/dkoosis/strand/internal/server"
	"github.com/dkoosis/strand/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	dir := flag.String("dir", "", "beads workspace directory (default: current directory)")
	bin := flag.String("bd", "bd", "path to the bd binary")
	northStar := flag.String("northstar", "", "north-star line shown above the forest")
	flag.Parse()

	tmpl, err := web.Templates()
	if err != nil {
		log.Fatalf("strand: parse templates: %v", err)
	}

	client := &bd.Client{Dir: *dir, Bin: *bin}
	syn := forest.Synthesis{Project: projectLabel(*dir), NorthStar: *northStar}
	srv := server.New(client, tmpl, web.Static(), syn)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("strand: serving http://%s (beads dir: %s)", *addr, projectLabel(*dir))
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("strand: %v", err)
	}
}

// projectLabel names the forest region: the workspace directory's base name, or
// the current directory's when -dir is unset.
func projectLabel(dir string) string {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else {
			return "."
		}
	}
	return filepath.Base(dir)
}
