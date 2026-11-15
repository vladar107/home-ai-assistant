// Package pagesim is a loopback fake of public web pages for the web connector's fetch_page tool.
// It serves seeded pages by host and path, redirects, error statuses, and a page that never
// answers, so a case can test reading a page, following a link, and recovering from a dead site.
//
// The web connector only dials public addresses on standard ports, so the fake is mounted on a
// simkit.Router under the seeded host names, and the connector is given the router's resolver and
// dialer; the connector's address guard stays whole and still vets every hop.
//
// Example seed (fictional site):
//
//	{
//	  "pages": [
//	    {"host": "news.example", "path": "/market", "title": "Eastbrook market hours",
//	     "body": "<p>The Eastbrook market opens at 9:00 on Saturdays.</p>"},
//	    {"host": "news.example", "path": "/old", "redirect_to": "http://news.example/market"},
//	    {"host": "news.example", "path": "/gone", "status": 404},
//	    {"host": "news.example", "path": "/stuck", "timeout": true}
//	  ]
//	}
//
// A page's content type defaults to text/html; a body without a content type is wrapped in a
// minimal HTML document with the title.
package pagesim

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const DefaultHost = "pages.example"

type Seed struct {
	Pages []Page `json:"pages"`
}

type Page struct {
	// Host defaults to DefaultHost.
	Host string `json:"host"`
	Path string `json:"path"`
	// Status defaults to 200.
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	RedirectTo  string `json:"redirect_to"`
	// Timeout makes the page accept the request and never answer, until the client gives up.
	Timeout bool `json:"timeout"`
}

// Validate checks that seed has the shape the fake accepts.
func Validate(seed json.RawMessage) error {
	_, err := parse(seed)
	return err
}

func parse(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	seen := map[string]bool{}
	for index := range seed.Pages {
		page := &seed.Pages[index]
		if page.Host == "" {
			page.Host = DefaultHost
		}
		if !strings.HasPrefix(page.Path, "/") {
			return seed, fmt.Errorf("pages[%d] path must start with /", index)
		}
		if page.Status != 0 && (page.Status < 100 || page.Status > 599) {
			return seed, fmt.Errorf("pages[%d] status is not an HTTP status", index)
		}
		if key := page.Host + page.Path; seen[key] {
			return seed, fmt.Errorf("pages[%d] repeats %s", index, key)
		} else {
			seen[key] = true
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	pages map[string]Page
	hosts []string
}

// Start runs the fake on a loopback port; the caller closes it.
func Start(raw json.RawMessage) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	s := &Server{pages: map[string]Page{}}
	for _, page := range seed.Pages {
		s.pages[page.Host+page.Path] = page
		if !contains(s.hosts, page.Host) {
			s.hosts = append(s.hosts, page.Host)
		}
	}
	inner, err := simkit.Serve(http.HandlerFunc(s.serve))
	if err != nil {
		return nil, err
	}
	s.Server = inner
	return s, nil
}

// New runs the fake for the length of a test.
func New(t testing.TB, raw json.RawMessage) *Server {
	t.Helper()
	s, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup(t)
	return s
}

func contains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}
	return false
}

// Mount routes every seeded host to the fake.
func (s *Server) Mount(router *simkit.Router) {
	for _, host := range s.hosts {
		router.Add(host, strings.TrimPrefix(s.URL(), "http://"))
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	page, found := s.pages[r.Host+r.URL.Path]
	if !found {
		http.NotFound(w, r)
		return
	}
	switch {
	case page.Timeout:
		<-r.Context().Done()
	case page.RedirectTo != "":
		http.Redirect(w, r, page.RedirectTo, cmpStatus(page.Status, http.StatusFound))
	default:
		contentType, body := page.ContentType, page.Body
		if contentType == "" {
			contentType = "text/html; charset=utf-8"
			body = "<!doctype html><html><head><title>" + html.EscapeString(page.Title) + "</title></head><body>" + page.Body + "</body></html>"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(cmpStatus(page.Status, http.StatusOK))
		_, _ = w.Write([]byte(body))
	}
}

func cmpStatus(status, fallback int) int {
	if status == 0 {
		return fallback
	}
	return status
}
