package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"
)

const maxPasteBytes = 1 << 20 // 1 MiB

type server struct {
	store Store
	tmpl  *template.Template
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openStore opens the paste store wrapped in encryption, and returns a
// description of where it lives. Postgres is opt-in via db; by default pastes
// go to pastes.jsonl in dataDir, with nothing to configure. readOnly (the CLI)
// never creates files or keys.
func openStore(db string, readOnly bool) (Store, string, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, "", err
	}
	var inner Store
	var where string
	hasData := false
	if db != "" {
		inner, err = openPG(context.Background(), db)
		where = "postgres"
	} else {
		path := filepath.Join(dir, "pastes.jsonl")
		if !readOnly {
			err = os.MkdirAll(dir, 0o700)
		}
		if err == nil {
			if fi, serr := os.Stat(path); serr == nil && fi.Size() > 0 {
				hasData = true
			}
			inner, err = openFile(path, readOnly)
			if readOnly && errors.Is(err, fs.ErrNotExist) {
				err = fmt.Errorf("no pastes yet: %s doesn't exist", path)
			}
		}
		where = path
	}
	if err != nil {
		return nil, "", err
	}

	key, err := loadKey(dir)
	if errors.Is(err, errNoKey) {
		switch {
		case readOnly:
			err = fmt.Errorf("no key in %s: start the server once to create it", filepath.Join(dir, ".env"))
		case hasData:
			// a fresh key would make every existing paste unreadable
			err = fmt.Errorf("%s has pastes but there is no %s in %s or the environment; restore it", where, keyVar, filepath.Join(dir, ".env"))
		default:
			key, err = newKey(dir)
		}
	}
	if err != nil {
		inner.Close()
		return nil, "", err
	}
	s, err := newSealed(inner, key)
	if err != nil {
		inner.Close()
		return nil, "", err
	}
	return s, where, nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ls", "cat":
			runCLI(os.Args[1], os.Args[2:])
			return
		case "help":
			fmt.Println(cliUsage)
			return
		}
	}
	os.Exit(serve())
}

// serve runs the HTTP server until it fails or gets SIGINT/SIGTERM, and
// returns the exit code. It's separate from main so deferred calls (store
// Close) run; os.Exit skips them.
func serve() int {
	addr := flag.String("addr", env("ADDR", ":8000"), "listen address, e.g. :8000 or 8000")
	db := flag.String("db", os.Getenv("DATABASE_URL"), "Postgres connection string (optional; default is a local file)")
	logFmt := flag.String("log", env("LOG_FORMAT", "text"), "log format: text or json")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "psbin: unknown command %q\n\n%s\n", flag.Arg(0), cliUsage)
		return 2
	}
	if !strings.Contains(*addr, ":") {
		*addr = ":" + *addr
	}

	// the std log package goes through the default slog handler too
	opts := &slog.HandlerOptions{}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if *logFmt == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))

	store, where, err := openStore(*db, false)
	if err != nil {
		slog.Error("open store", "err", err)
		return 1
	}
	defer store.Close()
	slog.Info("storing encrypted pastes", "in", where)

	s := &server{store: store, tmpl: template.Must(template.New("").Parse(pages))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /paste", s.create)
	mux.HandleFunc("GET /p/{id}", s.view)
	mux.HandleFunc("GET /p/{id}/raw", s.raw)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	printURL(*addr)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		slog.Error("server stopped", "err", err)
		return 1
	case <-ctx.Done():
		slog.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			slog.Error("shutdown", "err", err)
			return 1
		}
		return 0
	}
}

// printURL prints the LAN URL and a QR code for it, so a phone on the
// same wifi can scan instead of typing the IP.
func printURL(addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Info("listening", "addr", addr)
		return
	}
	// dialing UDP sends no packets; it just makes the kernel pick the
	// outbound interface, whose address is the LAN IP.
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		slog.Info("listening (no LAN IP found)", "url", "http://localhost:"+port)
		return
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP

	url := "http://" + net.JoinHostPort(ip.String(), port)
	slog.Info("listening", "url", url)
	if qr, err := qrcode.New(url, qrcode.Low); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}
}

// fail logs err and sends a generic 500, so store errors don't leak to clients.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "template", name, "err", err)
	}
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	recent, err := s.store.Recent(r.Context(), 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, "index", recent)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes+1024)
	// browsers submit textarea line breaks as \r\n
	content := strings.ReplaceAll(r.FormValue("content"), "\r\n", "\n")
	if content == "" {
		slog.Warn("rejected paste", "remote", r.RemoteAddr, "reason", "empty or over 1 MiB")
		http.Error(w, "empty paste (or larger than 1 MiB)", http.StatusBadRequest)
		return
	}
	id := newID()
	if err := s.store.Create(r.Context(), id, content); err != nil {
		s.fail(w, r, err)
		return
	}
	slog.Info("paste created", "id", id, "bytes", len(content), "remote", r.RemoteAddr)
	http.Redirect(w, r, "/p/"+id, http.StatusSeeOther)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) (paste, bool) {
	p, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return p, false
	}
	if err != nil {
		s.fail(w, r, err)
		return p, false
	}
	return p, true
}

func (s *server) view(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.get(w, r); ok {
		s.render(w, "view", p)
	}
}

func (s *server) raw(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.get(w, r); ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(p.Content))
	}
}

const pages = `
{{define "head"}}<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>psbin</title>
<style>
	body { font: 15px/1.5 monospace; max-width: 52rem; margin: 2rem auto; padding: 0 1rem; }
	textarea { width: 100%; height: 22rem; font: inherit; box-sizing: border-box; }
	pre { white-space: pre-wrap; word-break: break-word; background: #8881; padding: 1rem; }
	li { margin: .25rem 0; }
	small { opacity: .6; }
</style>
<script>
// navigator.clipboard needs a secure context; plain http://<lan-ip> isn't one,
// so fall back to a hidden textarea + execCommand.
function copyText(t) {
	if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(t);
	return new Promise((ok, fail) => {
		const a = document.createElement('textarea');
		a.value = t;
		a.style.cssText = 'position:fixed;opacity:0';
		document.body.appendChild(a);
		a.select();
		a.setSelectionRange(0, a.value.length); // iOS
		try { document.execCommand('copy') ? ok() : fail(); } catch (e) { fail(e); }
		a.remove();
	});
}
document.addEventListener('click', async e => {
	const b = e.target.closest('button[data-copy]');
	if (!b) return;
	const src = b.dataset.copy;
	const label = b.textContent;
	try {
		const text = src === 'pre'
			? document.querySelector('pre').textContent
			: await (await fetch('/p/' + src + '/raw')).text();
		await copyText(text);
		b.textContent = 'copied';
	} catch (_) {
		b.textContent = 'failed';
	}
	setTimeout(() => b.textContent = label, 1200);
});
</script>
<h1><a href="/" style="color:inherit;text-decoration:none">psbin</a></h1>
{{end}}

{{define "index"}}{{template "head"}}
<form method="post" action="/paste">
	<textarea name="content" placeholder="paste anything" autofocus required></textarea>
	<p><button>save</button></p>
</form>
<h3>recent</h3>
<ul>
{{range .}}<li><button type="button" data-copy="{{.ID}}">copy</button> <a href="/p/{{.ID}}">{{.ID}}</a> <small>{{.CreatedAt.Format "02 Jan 15:04"}}</small> {{.Content}}</li>
{{else}}<li><small>nothing yet</small></li>
{{end}}</ul>
{{end}}

{{define "view"}}{{template "head"}}
<p><small>{{.ID}} · {{.CreatedAt.Format "02 Jan 2006 15:04"}}</small> · <a href="/p/{{.ID}}/raw">raw</a> · <button type="button" data-copy="pre">copy</button></p>
<pre>{{.Content}}</pre>
{{end}}
`
