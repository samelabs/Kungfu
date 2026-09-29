package server

// Platform admin (/samelabs) — server-rendered pages.
//
// Every page is rendered here from the admin domain (internal/admin);
// there is no client-side application. Reads are plain GETs; every
// mutation is an HTML form POST carrying the session-bound CSRF token
// in a hidden _csrf field, followed by a redirect (POST → 303 → GET)
// with a one-shot flash message. Permission checks stay in the admin
// domain; the page layer only hides navigation the principal cannot use.

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kungfu.md/internal/admin"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/repository"
	"kungfu.md/web"
)

const (
	slBase       = "/samelabs"
	slFlashName  = "kf_sl_flash"
	slFormLimit  = 64 << 10
	slPageSize   = 50
	slTimeLayout = "2006-01-02 15:04"
)

// -- templates --

var (
	slTemplatesOnce sync.Once
	slTemplates     map[string]*template.Template
)

func slFuncs() template.FuncMap {
	return template.FuncMap{
		"time": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format(slTimeLayout)
		},
		"ptime": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "—"
			}
			return t.UTC().Format(slTimeLayout)
		},
		"num": formatThousands,
		"minor": func(amount int64, currency string) string {
			sign := ""
			if amount < 0 {
				sign, amount = "-", -amount
			}
			return sign + formatThousands(amount/100) + "." + twoDigits(amount%100) + " " + strings.ToUpper(currency)
		},
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		// dash renders a nullable column for the finance tables: NULL is
		// a fact (legacy payments have no provider order, refunds may
		// carry no reason) and is shown as "—", never as "<nil>".
		"dash": func(s *string) string {
			if s == nil || *s == "" {
				return "—"
			}
			return *s
		},
		"prettyjson": func(b []byte) string {
			if len(b) == 0 {
				return ""
			}
			var out bytes.Buffer
			if err := json.Indent(&out, b, "", "  "); err != nil {
				return string(b)
			}
			return out.String()
		},
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
		"add":   func(a, b int) int { return a + b },
		"list":  func(xs ...string) []string { return xs },
		"trunc": truncateRunes,
		"pager": newSlPager,
	}
}

func slTemplate(name string) *template.Template {
	slTemplatesOnce.Do(func() {
		fsys := web.SamelabsTemplates()
		base := template.Must(template.New("layout.html").Funcs(slFuncs()).ParseFS(fsys, "layout.html"))
		pages, err := fs.Glob(fsys, "*.html")
		if err != nil {
			panic(err)
		}
		slTemplates = map[string]*template.Template{}
		for _, p := range pages {
			if p == "layout.html" {
				continue
			}
			t := template.Must(base.Clone())
			slTemplates[strings.TrimSuffix(p, ".html")] = template.Must(t.ParseFS(fsys, p))
		}
	})
	t, ok := slTemplates[name]
	if !ok {
		panic("samelabs: unknown template " + name)
	}
	return t
}

// -- view model --

type slFlash struct {
	Kind string // ok | error
	Text string
}

type slView struct {
	Title     string
	Nav       string
	Principal *admin.Principal
	CSRF      string
	Flash     *slFlash
	Query     url.Values
	Data      interface{}
	Error     string
}

// Can reports whether the signed-in admin holds a permission (nav only;
// the domain enforces every read and write independently).
func (v *slView) Can(perm string) bool {
	return v.Principal != nil && v.Principal.HasPermission(perm)
}

// Q returns one query parameter (filters keep their values).
func (v *slView) Q(key string) string { return v.Query.Get(key) }

type slPager struct {
	Page, Pages int
	Total       int64
	PrevURL     string
	NextURL     string
}

func newSlPager(q url.Values, total int64, page, size int) slPager {
	if size <= 0 {
		size = slPageSize
	}
	pages := int(math.Ceil(float64(total) / float64(size)))
	if pages < 1 {
		pages = 1
	}
	p := slPager{Page: page, Pages: pages, Total: total}
	link := func(n int) string {
		c := url.Values{}
		for k, v := range q {
			c[k] = v
		}
		c.Set("page", strconv.Itoa(n))
		return "?" + c.Encode()
	}
	if page > 1 {
		p.PrevURL = link(page - 1)
	}
	if page < pages {
		p.NextURL = link(page + 1)
	}
	return p
}

// -- request plumbing --

func slPage(q url.Values) int {
	n, err := strconv.Atoi(q.Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return clampPage(n, slPageSize)
}

// slInt64 parses an optional non-negative integer filter value. An
// ABSENT or empty value is "no filter" (0, true); a present value that
// is not a valid non-negative integer is an error the caller must
// answer with 400 — it must never silently degrade into "no filter"
// (an unfiltered list), matching the API plane's fail-closed contract.
func slInt64(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// slInt64Query is slInt64 over one query parameter, reporting a 400
// AppError naming the parameter when its value is present but invalid.
func slInt64Query(q url.Values, name string) (int64, error) {
	n, ok := slInt64(q.Get(name))
	if !ok {
		return 0, apperrors.New(400, "INVALID_PARAMETER", "invalid "+name)
	}
	return n, nil
}

func (s *Server) slSession(r *http.Request) (*admin.Principal, string) {
	c, err := r.Cookie(admin.AdminCookieName)
	if err != nil || c.Value == "" {
		return nil, ""
	}
	p, err := admin.ResolveSession(r.Context(), s.Pool, c.Value)
	if err != nil {
		return nil, ""
	}
	return p, c.Value
}

func slRedirectLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Path
	if r.Method == http.MethodGet && r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, slBase+"/login?next="+url.QueryEscape(next), http.StatusSeeOther)
}

// slSafeNext only allows in-admin relative targets (no open redirect).
func slSafeNext(next string) string {
	if next == slBase || (strings.HasPrefix(next, slBase+"/") && !strings.HasPrefix(next, "//")) {
		if !strings.Contains(next, "\\") && !strings.HasPrefix(next, slBase+"/login") {
			return next
		}
	}
	return slBase
}

func (s *Server) slSetFlash(w http.ResponseWriter, r *http.Request, kind, text string) {
	http.SetCookie(w, &http.Cookie{
		Name:     slFlashName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + truncateRunes(text, 300))),
		Path:     slBase,
		MaxAge:   60,
		HttpOnly: true,
		Secure:   middleware.IsHTTPS(r, s.TrustedProxies),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) slTakeFlash(w http.ResponseWriter, r *http.Request) *slFlash {
	c, err := r.Cookie(slFlashName)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: slFlashName, Value: "", Path: slBase, MaxAge: -1, HttpOnly: true,
		Secure: middleware.IsHTTPS(r, s.TrustedProxies), SameSite: http.SameSiteStrictMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, text, ok := strings.Cut(string(raw), "|")
	if !ok || (kind != "ok" && kind != "error") {
		return nil
	}
	return &slFlash{Kind: kind, Text: text}
}

func (s *Server) slRender(w http.ResponseWriter, status int, tmpl string, v *slView) {
	var buf bytes.Buffer
	if err := slTemplate(tmpl).ExecuteTemplate(&buf, "layout.html", v); err != nil {
		log.Printf("samelabs: render %s: %v", tmpl, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = w.Write(web.FingerprintHTML(buf.Bytes()))
}

// slErrorPage maps a domain error onto a page.
func (s *Server) slErrorPage(w http.ResponseWriter, v *slView, err error) {
	status, msg := http.StatusInternalServerError, "Something went wrong. The error was logged."
	if ae, ok := apperrors.IsAppError(err); ok {
		status, msg = ae.HTTPCode, ae.Message
		if status >= 500 {
			msg = "Something went wrong. The error was logged."
		}
	} else {
		log.Printf("samelabs: %v", err)
	}
	v.Error = msg
	switch status {
	case http.StatusForbidden:
		v.Title = "No access"
		s.slRender(w, status, "forbidden", v)
	case http.StatusNotFound:
		v.Title = "Not found"
		s.slRender(w, status, "error", v)
	default:
		v.Title = "Error"
		s.slRender(w, status, "error", v)
	}
}

type slLoader func(r *http.Request, p *admin.Principal) (interface{}, error)

// slGet builds a page handler: session required (else → login), then
// the loader's domain call; domain errors become error pages.
func (s *Server) slGet(nav, title, tmpl string, load slLoader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, raw := s.slSession(r)
		if p == nil {
			slRedirectLogin(w, r)
			return
		}
		v := &slView{
			Title: title, Nav: nav, Principal: p, Query: r.URL.Query(),
			CSRF:  admin.CSRFToken(raw, s.Config.SessionSecret),
			Flash: s.slTakeFlash(w, r),
		}
		data, err := load(r, p)
		if err != nil {
			s.slErrorPage(w, v, err)
			return
		}
		v.Data = data
		if t := slItemTitle(data); t != "" {
			v.Title = t
		}
		s.slRender(w, http.StatusOK, tmpl, v)
	}
}

type slAction func(r *http.Request, p *admin.Principal) (redirect, message string, err error)

// slPost builds a form action: session + CSRF, then the domain call,
// then 303 back with a flash (success or the domain error message).
func (s *Server) slPost(fn slAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, raw := s.slSession(r)
		if p == nil {
			slRedirectLogin(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, slFormLimit)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Form too large or malformed", http.StatusBadRequest)
			return
		}
		want := admin.CSRFToken(raw, s.Config.SessionSecret)
		if !hmac.Equal([]byte(r.PostFormValue("_csrf")), []byte(want)) {
			v := &slView{Title: "Session expired", Principal: p, CSRF: want, Query: url.Values{},
				Error: "This form expired or was submitted from somewhere else. Go back, reload the page and try again."}
			s.slRender(w, http.StatusForbidden, "error", v)
			return
		}
		back, msg, err := fn(r, p)
		if back == "" {
			back = slBase
		}
		if err != nil {
			text := "Something went wrong. The error was logged."
			if ae, ok := apperrors.IsAppError(err); ok && ae.HTTPCode < 500 {
				text = ae.Message
			} else {
				log.Printf("samelabs: action %s: %v", r.URL.Path, err)
			}
			s.slSetFlash(w, r, "error", text)
		} else if msg != "" {
			s.slSetFlash(w, r, "ok", msg)
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

// slItemTitle names detail pages after the item they show.
func slItemTitle(data interface{}) string {
	switch d := data.(type) {
	case *repository.AdminMemoryRow:
		return d.Title
	case slAccountData:
		return "@" + d.Account.BotName
	case *model.RewardsProduct:
		return d.Title
	case *model.Redemption:
		return "Redemption " + d.Code
	case *admin.FinancePaymentDetail:
		return "Payment " + d.Payment.Code
	case slAdminData:
		return d.User.Admin.DisplayName
	case slRoleData:
		return d.Role.Role.Name
	case slTaskData:
		if d.Detail.Task.Title != "" {
			return d.Detail.Task.Title
		}
		return "Task " + d.Detail.Task.Code
	}
	return ""
}

// -- formatting helpers --

func formatThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func twoDigits(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
