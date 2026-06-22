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
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/server"
	"github.com/dkoosis/strand/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	dir := flag.String("dir", "", "seed this beads workspace into the registry and make it active")
	bin := flag.String("bd", "bd", "path to the bd binary")
	northStar := flag.String("northstar", "", "north-star line shown above the forest")
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
	syn := forest.Synthesis{NorthStar: *northStar}
	srv := server.New(srcFor, reg, tmpl, web.Static(), syn)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("strand: serving http://%s", *addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("strand: %v", err)
	}
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
