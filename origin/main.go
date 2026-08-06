package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/yzp0n/ncdn/httprps"
)

var nodeId = flag.String("nodeId", "unknown_node", "Name of the node")
var listenAddr = flag.String("listenAddr", ":8888", "Address to listen on")

type requestInfo struct {
	RemoteAddr string
	PopCacheId string
	OriginId   string
}

func dumpRequestInfo(r *http.Request) requestInfo {
	return requestInfo{
		RemoteAddr: r.RemoteAddr,
		PopCacheId: r.Header.Get("X-NCDN-PoPCache-NodeId"),
		OriginId:   *nodeId,
	}
}

func serveIndexHTMLInternal(w http.ResponseWriter, r *http.Request) error {
	tmpl, err := template.New("index.html.gotmpl").ParseFiles("./templates/index.html.gotmpl")
	if err != nil {
		return fmt.Errorf("Failed to parse index.html template: %w", err)
	}

	ri := dumpRequestInfo(r)

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, &ri); err != nil {
		return fmt.Errorf("Failed to execute index.html template: %w", err)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "max-age=60, stale-while-validate=120, stale-if-error=180")
	_, err = w.Write(buf.Bytes())
	if err != nil {
		log.Printf("Failed to write response: %v", err)
		return nil // since it is too late to recover
	}

	return nil
}

func serveIndexHTML(w http.ResponseWriter, r *http.Request) {
	err := serveIndexHTMLInternal(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serveJsonInternal(w http.ResponseWriter, r *http.Request) error {
	ri := dumpRequestInfo(r)

	bs, err := json.MarshalIndent(ri, "", "  ")
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(bs)
	if err != nil {
		log.Printf("Failed to write response: %v", err)
		return nil // since it is too late to recover
	}

	return nil
}

func serveJson(w http.ResponseWriter, r *http.Request) {
	err := serveJsonInternal(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
func withCacheControl(next http.Handler, value string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}
func main() {
	flag.Parse()

	fs := withCacheControl(
		http.FileServer(http.Dir("./static")),
		"public, max-age=3600",
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/number/{num}", func(w http.ResponseWriter, r *http.Request) {
		num := r.PathValue("num")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "max-age=5, stale-while-revalidate=15, stale-if-error=60")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Hello %s\nDate: %s\n", num, time.Now().String())
	})
	mux.HandleFunc("/index.html", serveIndexHTML)
	mux.HandleFunc("/json", serveJson)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// redirect to index.html
			http.Redirect(w, r, "/index.html", http.StatusPermanentRedirect)
			return
		}

		fs.ServeHTTP(w, r)
	})

	rps := httprps.NewMiddleware(mux)
	http.Handle("/", rps)
	mux.HandleFunc("/rps", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "RPS: %.2f\n", rps.GetRPS())
	})

	log.Printf("Listening on %s...\n", *listenAddr)
	err := http.ListenAndServe(*listenAddr, nil)
	if err != nil {
		log.Fatal(err)
	}
}
