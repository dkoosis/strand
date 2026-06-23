# goroutine-lifecycle — strand (repo, project scope)

RUN_ID: 2af4cc879761 · mode: report

Surface is tiny: `go 1.26.4` (loop-var capture fixed), one production goroutine
(`cmd/strand/main.go:74`), one test goroutine (`internal/graph/graph_test.go:107`).
The Quit-button shutdown path flows the UI through the same `signal.NotifyContext`
seam as Ctrl-C (`internal/server/server.go:99-103` → main's waiter), which is clean.
No background goroutines in `New()` constructors, no closures over shared pointers,
no `time.Sleep` as sync, no magic channel buffers in prod.

One borderline finding.

---

### 1. [F1] `cmd/strand/main.go:74` — goroutine-no-owner

**Diagnosis:** The shutdown goroutine drains `httpSrv.Shutdown` with no handshake
back to `main`. `main` returns — and the process exits — the instant `Serve`
returns `http.ErrServerClosed`, which fires the moment `Shutdown` closes the
listener, not when the in-flight drain completes. Nothing waits for the drain.

**Why:** The header comment promises "in-flight requests get a moment to finish":

```
70	// silent drop, and in-flight requests get a moment to finish.
```

But `Serve` unblocks at the *start* of `Shutdown`, not its end. So `main` falls
out of the function while the goroutine is still inside its 5s `Shutdown` drain.
On normal exit Go does not wait for live goroutines — the process can terminate
mid-drain, dropping the very in-flight requests the comment says it protects. The
documented graceful guarantee is unenforced. The spec dampens process-lifetime
`main` goroutines (note-as-info), so this lands at borderline; the defect is the
missing owner/handshake, not the goroutine's lifetime.

**Evidence:** `cmd/strand/main.go`:

```
74		go func() {
75			<-ctx.Done()
76			log.Printf("strand: shutting down — bye")
77			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
78			defer cancel()
79			if err := httpSrv.Shutdown(shutCtx); err != nil {
80				log.Printf("strand: shutdown: %v", err)
81			}
82		}()
```

```
98		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
99			log.Fatalf("strand: %v", err)
100		}
101	}
```

`Serve` returns `ErrServerClosed` → `main` returns → no `Wait()`/`<-done` couples
it to the goroutine on line 74.

**Fix:** Give the goroutine an owner. Signal completion and block `main` on it
before returning:

```go
shutdownDone := make(chan struct{})
go func() {
	defer close(shutdownDone)
	<-ctx.Done()
	log.Printf("strand: shutting down — bye")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Printf("strand: shutdown: %v", err)
	}
}()
// ...after Serve returns ErrServerClosed:
<-shutdownDone
```

Now `main` waits for the drain it promises. The inner `context.Background()` on
line 77 is correct as-is — shutdown must outlive the cancelled parent ctx — so
leave it.

**Tier:** borderline
