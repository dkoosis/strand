// Command strand serves a human-friendly web UI over a beads (bd) workspace.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/server"
	"github.com/dkoosis/strand/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	dir := flag.String("dir", "", "beads workspace directory (default: current directory)")
	bin := flag.String("bd", "bd", "path to the bd binary")
	flag.Parse()

	client := &bd.Client{Dir: *dir, Bin: *bin}
	srv := server.New(client, http.FileServer(http.FS(web.FS)))

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("strand: serving http://%s (beads dir: %s)", *addr, dirLabel(*dir))
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("strand: %v", err)
	}
}

func dirLabel(dir string) string {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	return dir
}
