package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skip2/go-qrcode"
)

const maxPasteBytes = 1 << 20 // 1 MiB

const schema = `
CREATE TABLE IF NOT EXISTS pastes (
	id         TEXT PRIMARY KEY,
	content    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

type paste struct {
	ID        string
	Content   string
	CreatedAt time.Time
}

type server struct {
	db   *pgxpool.Pool
	tmpl *template.Template
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "host=/run/postgresql dbname=textshare"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8000"
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, schema); err != nil {
		log.Fatal(err)
	}

	s := &server{db: db, tmpl: template.Must(template.New("").Parse(pages))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /paste", s.create)
	mux.HandleFunc("GET /p/{id}", s.view)
	mux.HandleFunc("GET /p/{id}/raw", s.raw)

	printURL(addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// printURL prints the LAN URL and a QR code for it, so a phone on the
// same wifi can scan instead of typing the IP.
func printURL(addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("listening on %s", addr)
		return
	}
	// dialing UDP sends no packets; it just makes the kernel pick the
	// outbound interface, whose address is the LAN IP.
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Printf("listening on http://localhost:%s (no LAN IP found)", port)
		return
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP

	url := "http://" + net.JoinHostPort(ip.String(), port)
	log.Printf("listening on %s", url)
	if qr, err := qrcode.New(url, qrcode.Low); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(),
		`SELECT id, left(content, 80), created_at FROM pastes ORDER BY created_at DESC LIMIT 20`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recent, err := pgx.CollectRows(rows, pgx.RowToStructByPos[paste])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.tmpl.ExecuteTemplate(w, "index", recent)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes+1024)
	content := r.FormValue("content")
	if content == "" {
		http.Error(w, "empty paste (or larger than 1 MiB)", http.StatusBadRequest)
		return
	}
	id := newID()
	_, err := s.db.Exec(r.Context(),
		`INSERT INTO pastes (id, content) VALUES ($1, $2)`, id, content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/p/"+id, http.StatusSeeOther)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) (paste, bool) {
	var p paste
	err := s.db.QueryRow(r.Context(),
		`SELECT id, content, created_at FROM pastes WHERE id = $1`, r.PathValue("id")).
		Scan(&p.ID, &p.Content, &p.CreatedAt)
	if err == pgx.ErrNoRows {
		http.NotFound(w, r)
		return p, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return p, false
	}
	return p, true
}

func (s *server) view(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.get(w, r); ok {
		s.tmpl.ExecuteTemplate(w, "view", p)
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
<title>text-share</title>
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
<h1><a href="/" style="color:inherit;text-decoration:none">text-share</a></h1>
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
