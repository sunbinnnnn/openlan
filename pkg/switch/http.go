package _switch

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/luscis/openlan/pkg/api"
	"github.com/luscis/openlan/pkg/cache"
	co "github.com/luscis/openlan/pkg/config"
	"github.com/luscis/openlan/pkg/libol"
	"github.com/luscis/openlan/pkg/models"
	"github.com/luscis/openlan/pkg/schema"
	"io/ioutil"
	"net/http"
	"net/http/pprof"
	"os"
	"path"
	"sort"
	"strings"
	"text/template"
	"time"
)

type Http struct {
	switcher   api.Switcher
	listen     string
	adminToken string
	guestToken string
	adminFile  string
	server     *http.Server
	crtFile    string
	keyFile    string
	pubDir     string
	router     *mux.Router
}

func NewHttp(switcher api.Switcher) (h *Http) {
	c := co.Manager.Switch
	h = &Http{
		switcher:  switcher,
		listen:    c.Http.Listen,
		adminFile: c.TokenFile,
		pubDir:    c.Http.Public,
	}
	if c.Cert != nil {
		h.crtFile = c.Cert.CrtFile
		h.keyFile = c.Cert.KeyFile
	}
	return
}

func (h *Http) Initialize() {
	r := h.Router()
	if h.server == nil {
		h.server = &http.Server{
			Addr:         h.listen,
			Handler:      r,
			ReadTimeout:  5 * time.Minute,
			WriteTimeout: 10 * time.Minute,
		}
	}
	h.LoadToken()
	h.SaveToken()
	h.LoadRouter()
}

func (h *Http) PProf(r *mux.Router) {
	if r != nil {
		r.HandleFunc("/debug/pprof/", pprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
}

func (h *Http) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		libol.Info("Http.Middleware %s %s", r.Method, r.URL.Path)
		if h.IsAuth(w, r) {
			next.ServeHTTP(w, r)
		} else {
			w.Header().Set("WWW-Authenticate", "Basic")
			http.Error(w, "Authorization Required", http.StatusUnauthorized)
		}
	})
}

func (h *Http) Router() *mux.Router {
	if h.router == nil {
		h.router = mux.NewRouter()
		h.router.Use(h.Middleware)
	}

	return h.router
}

func (h *Http) SaveToken() {
	f, err := os.OpenFile(h.adminFile, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0600)
	if err != nil {
		libol.Error("Http.SaveToken: %s", err)
		return
	}
	defer f.Close()
	if _, err := f.Write([]byte(h.adminToken)); err != nil {
		libol.Error("Http.SaveToken: %s", err)
		return
	}
}

func (h *Http) LoadRouter() {
	router := h.Router()

	router.HandleFunc("/", h.IndexHtml)
	router.HandleFunc("/index.html", h.IndexHtml)
	router.HandleFunc("/favicon.ico", h.PubFile)

	h.PProf(router)
	router.HandleFunc("/api/index", h.GetIndex).Methods("GET")
	api.Add(router, h.switcher)
}

func (h *Http) LoadToken() {
	token := ""
	if _, err := os.Stat(h.adminFile); os.IsNotExist(err) {
		libol.Info("Http.LoadToken: file:%s does not exist", h.adminFile)
	} else {
		contents, err := ioutil.ReadFile(h.adminFile)
		if err != nil {
			libol.Error("Http.LoadToken: file:%s %s", h.adminFile, err)
		} else {
			token = strings.TrimSpace(string(contents))
		}
	}
	if token == "" {
		token = libol.GenRandom(32)
	}
	h.SetToken(token)
}

func (h *Http) SetToken(value string) {
	sum := md5.Sum([]byte(value))
	h.adminToken = value
	h.guestToken = hex.EncodeToString(sum[:16])[:12]
}

func (h *Http) Start() {
	h.Initialize()

	libol.Info("Http.Start %s", h.listen)
	promise := &libol.Promise{
		First:  time.Second * 2,
		MaxInt: time.Minute,
		MinInt: time.Second * 10,
	}
	promise.Done(func() error {
		if h.keyFile == "" || h.crtFile == "" {
			if err := h.server.ListenAndServe(); err != nil {
				libol.Error("Http.Start on %s: %s", h.listen, err)
				return err
			}
		} else {
			if err := h.server.ListenAndServeTLS(h.crtFile, h.keyFile); err != nil {
				libol.Error("Http.Start on %s: %s", h.listen, err)
				return err
			}
		}
		return nil
	})
}

func (h *Http) Shutdown() {
	libol.Info("Http.Shutdown %s", h.listen)
	if err := h.server.Shutdown(context.Background()); err != nil {
		// Error from closing listeners, or context timeout:
		libol.Error("Http.Shutdown: %v", err)
	}
}

func (h *Http) IsAuth(w http.ResponseWriter, r *http.Request) bool {
	token, pass, ok := r.BasicAuth()
	libol.Debug("Http.IsAuth token: %s, pass: %s", token, pass)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !ok || token != h.adminToken {
			return false
		}
	} else if strings.HasPrefix(r.URL.Path, "/get/") {
		if !ok || token != h.guestToken {
			return false
		}
	}
	return true
}

func (h *Http) getFile(name string) string {
	return fmt.Sprintf("%s%s", h.pubDir, name)
}

func (h *Http) PubFile(w http.ResponseWriter, r *http.Request) {
	realpath := h.getFile(r.URL.Path)
	contents, err := ioutil.ReadFile(realpath)
	if err != nil {
		_, _ = fmt.Fprintf(w, "404")
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", contents)
}

func (h *Http) getIndex(body *schema.Index) *schema.Index {
	body.Version = schema.NewVersionSchema()
	body.Worker = api.NewWorkerSchema(h.switcher)

	// display accessed point.
	for p := range cache.Point.List() {
		if p == nil {
			break
		}
		body.Points = append(body.Points, models.NewPointSchema(p))
	}
	sort.SliceStable(body.Points, func(i, j int) bool {
		ii := body.Points[i]
		jj := body.Points[j]
		return ii.Network+ii.Remote > jj.Network+jj.Remote
	})
	// display neighbor.
	for n := range cache.Neighbor.List() {
		if n == nil {
			break
		}
		body.Neighbors = append(body.Neighbors, models.NewNeighborSchema(n))
	}
	sort.SliceStable(body.Neighbors, func(i, j int) bool {
		return body.Neighbors[i].IpAddr > body.Neighbors[j].IpAddr
	})
	// display links.
	for l := range cache.Link.List() {
		if l == nil {
			break
		}
		body.Links = append(body.Links, models.NewLinkSchema(l))
	}
	sort.SliceStable(body.Links, func(i, j int) bool {
		ii := body.Links[i]
		jj := body.Links[j]
		return ii.Network+ii.Server > jj.Network+jj.Server
	})
	// display online flow.
	for l := range cache.Online.List() {
		if l == nil {
			break
		}
		body.OnLines = append(body.OnLines, models.NewOnLineSchema(l))
	}
	sort.SliceStable(body.OnLines, func(i, j int) bool {
		return body.OnLines[i].HitTime < body.OnLines[j].HitTime
	})
	// display OpenVPN Clients.
	for n := range cache.Network.List() {
		if n == nil {
			break
		}
		for c := range cache.VPNClient.List(n.Name) {
			if c == nil {
				break
			}
			body.Clients = append(body.Clients, *c)
		}
		sort.SliceStable(body.Clients, func(i, j int) bool {
			return body.Clients[i].Name < body.Clients[j].Name
		})
	}
	// display esp state
	for s := range cache.EspState.List("") {
		if s == nil {
			break
		}
		body.States = append(body.States, models.NewEspStateSchema(s))
	}
	sort.SliceStable(body.States, func(i, j int) bool {
		ii := body.States[i]
		jj := body.States[j]
		return ii.Spi > jj.Spi
	})
	return body
}

func (h *Http) ParseFiles(w http.ResponseWriter, name string, data interface{}) error {
	file := path.Base(name)
	tmpl, err := template.New(file).Funcs(template.FuncMap{
		"prettyTime":  libol.PrettyTime,
		"prettyBytes": libol.PrettyBytes,
		"getIpAddr":   libol.GetIPAddr,
	}).ParseFiles(name)
	if err != nil {
		_, _ = fmt.Fprintf(w, "template.ParseFiles %s", err)
		return err
	}
	if err := tmpl.Execute(w, data); err != nil {
		_, _ = fmt.Fprintf(w, "template.ParseFiles %s", err)
		return err
	}
	return nil
}

func (h *Http) IndexHtml(w http.ResponseWriter, r *http.Request) {
	body := schema.Index{
		Points:    make([]schema.Point, 0, 128),
		Links:     make([]schema.Link, 0, 128),
		Neighbors: make([]schema.Neighbor, 0, 128),
		OnLines:   make([]schema.OnLine, 0, 128),
		Clients:   make([]schema.VPNClient, 0, 128),
		States:    make([]schema.EspState, 0, 128),
	}
	h.getIndex(&body)
	file := h.getFile("/index.html")
	if err := h.ParseFiles(w, file, &body); err != nil {
		libol.Error("Http.Index %s", err)
	}
}

func (h *Http) GetIndex(w http.ResponseWriter, r *http.Request) {
	body := schema.Index{
		Points:    make([]schema.Point, 0, 128),
		Links:     make([]schema.Link, 0, 128),
		Neighbors: make([]schema.Neighbor, 0, 128),
		OnLines:   make([]schema.OnLine, 0, 128),
		Network:   make([]schema.Network, 0, 128),
		Clients:   make([]schema.VPNClient, 0, 128),
	}
	h.getIndex(&body)
	api.ResponseJson(w, body)
}
