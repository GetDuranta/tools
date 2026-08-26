package devenvgateway

import (
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const csrfCookieName = "__Host-duranta-devenv-csrf"

var environmentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

var environmentListTemplate = template.Must(template.New("list").Funcs(template.FuncMap{
	"path":     url.PathEscape,
	"openable": func(state string) bool { return state == "RUNNING" },
	"startable": func(state string) bool {
		return state == "STOPPED" || state == "ARCHIVED" || state == "ERROR"
	},
	"stoppable": func(state string) bool { return state == "RUNNING" },
	"time": func(value time.Time) string {
		if value.IsZero() {
			return ""
		}
		return value.Format(time.RFC3339)
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Duranta dev environments</title></head><body><main><h1>Duranta dev environments</h1><p>Signed in as {{.Identity.Email}}</p>{{if .Environments}}{{range .Environments}}<article><h2>{{.Name}}</h2><p><code>{{.Host}}</code> · {{.State}}{{if .ExpiresAt}} · expires {{time .ExpiresAt}}{{end}}{{if .OwnerEmail}} · owner {{.OwnerEmail}}{{end}}</p>{{if openable .State}}<p><a href="/__auth/open?host={{urlquery .Host}}&return=%2Fa%2F">Open</a></p><form method="post" action="/env/{{path .ID}}/extend"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="idempotency" value="{{.ExtendKey}}"><button name="hours" value="1">Extend 1h</button> <button name="hours" value="4">Extend 4h</button></form>{{end}}{{if stoppable .State}}<form method="post" action="/env/{{path .ID}}/stop"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="idempotency" value="{{.StateKey}}"><button>Stop</button></form>{{else if startable .State}}<form method="post" action="/env/{{path .ID}}/start"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="idempotency" value="{{.StateKey}}"><button>Start</button></form>{{end}}<p><a href="/env/{{path .ID}}/confirm?action=archive">Archive…</a> <a href="/env/{{path .ID}}/confirm?action=delete">Delete…</a></p></article><hr>{{end}}{{else}}<p>No environments.</p>{{end}}<h2>Checkpoints</h2>{{if .Checkpoints}}{{range .Checkpoints}}<article><h3>{{.Name}}</h3><p><code>{{.ID}}</code> · {{.State}}{{if .Pinned}} · pinned{{end}}{{if .ExpiresAt}} · expires {{time .ExpiresAt}}{{end}}</p><p><a href="/checkpoint/{{path .ID}}/confirm">Delete…</a></p></article>{{end}}{{else}}<p>No checkpoints.</p>{{end}}</main></body></html>`))

var confirmationTemplate = template.Must(template.New("confirm").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Confirm action</title></head><body><main><h1>Confirm {{.Action}}</h1><p>{{.Name}} (<code>{{.Host}}</code>)</p><form method="post" action="/env/{{.ID}}/{{.Action}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="idempotency" value="{{.IdempotencyKey}}"><button>Confirm {{.Action}}</button> <a href="/">Cancel</a></form></main></body></html>`))

var checkpointConfirmationTemplate = template.Must(template.New("checkpoint-confirm").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Confirm checkpoint deletion</title></head><body><main><h1>Confirm checkpoint deletion</h1><p>{{.Name}} (<code>{{.ID}}</code>)</p><form method="post" action="/checkpoint/{{.ID}}/delete"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="idempotency" value="{{.IdempotencyKey}}"><button>Delete checkpoint</button> <a href="/">Cancel</a></form></main></body></html>`))

type environmentView struct {
	EnvironmentSummary
	ExtendKey string
	StateKey  string
}

type environmentListPage struct {
	Identity     Identity
	Environments []environmentView
	Checkpoints  []CheckpointSummary
	CSRF         string
}

type confirmationPage struct {
	ID             string
	Name           string
	Host           string
	Action         string
	CSRF           string
	IdempotencyKey string
}

type checkpointConfirmationPage struct {
	ID             string
	Name           string
	CSRF           string
	IdempotencyKey string
}

func (g *Gateway) serveControlUI(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.Path == "/" && request.Method == http.MethodGet {
		identity, err := g.controlIdentity(request)
		if err != nil {
			writeNotFound(writer)
			return true
		}
		environments, err := g.controlPlane.List(request.Context(), identity)
		if err != nil {
			g.logFailure("list environments", err)
			writeNotFound(writer)
			return true
		}
		checkpoints, err := g.controlPlane.ListCheckpoints(request.Context(), identity)
		if err != nil {
			g.logFailure("list checkpoints", err)
			writeNotFound(writer)
			return true
		}
		views := make([]environmentView, 0, len(environments))
		for _, environment := range environments {
			extendKey, keyErr := g.newActionKey()
			if keyErr != nil {
				writeNotFound(writer)
				return true
			}
			stateKey, keyErr := g.newActionKey()
			if keyErr != nil {
				writeNotFound(writer)
				return true
			}
			views = append(views, environmentView{
				EnvironmentSummary: environment, ExtendKey: extendKey, StateKey: stateKey,
			})
		}
		csrf, err := g.ensureCSRF(writer, request)
		if err != nil {
			writeNotFound(writer)
			return true
		}
		writeHTMLHeaders(writer)
		if environmentListTemplate.Execute(writer, environmentListPage{
			Identity: identity, Environments: views, Checkpoints: checkpoints, CSRF: csrf,
		}) != nil {
			return true
		}
		return true
	}

	checkpointID, checkpointConfirmation, checkpointOK := parseCheckpointControlPath(request.URL.Path, request.Method)
	if checkpointOK {
		identity, err := g.controlIdentity(request)
		if err != nil {
			writeNotFound(writer)
			return true
		}
		if checkpointConfirmation {
			g.renderCheckpointConfirmation(writer, request, identity, checkpointID)
			return true
		}
		if !g.validControlPOST(writer, request) {
			writeNotFound(writer)
			return true
		}
		idempotencyKey := request.Form.Get("idempotency")
		if !idempotencyKeyPattern.MatchString(idempotencyKey) {
			writeNotFound(writer)
			return true
		}
		if err = g.controlPlane.DeleteCheckpoint(request.Context(), identity, checkpointID,
			idempotencyKey); err != nil {
			g.logFailure("checkpoint action", err)
			writeNotFound(writer)
			return true
		}
		writer.Header().Set("Cache-Control", "no-store")
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return true
	}

	id, action, confirmation, ok := parseControlPath(request.URL.Path, request.Method, request.URL.Query())
	if !ok {
		return false
	}
	identity, err := g.controlIdentity(request)
	if err != nil {
		writeNotFound(writer)
		return true
	}
	if confirmation {
		g.renderConfirmation(writer, request, identity, id, action)
		return true
	}
	if !g.validControlPOST(writer, request) {
		writeNotFound(writer)
		return true
	}
	err = g.runControlAction(request, identity, id, action)
	if err != nil {
		g.logFailure("environment action", err)
		writeNotFound(writer)
		return true
	}
	writer.Header().Set("Cache-Control", "no-store")
	http.Redirect(writer, request, "/", http.StatusSeeOther)
	return true
}

func (g *Gateway) renderCheckpointConfirmation(writer http.ResponseWriter, request *http.Request,
	identity Identity, id string) {
	checkpoints, err := g.controlPlane.ListCheckpoints(request.Context(), identity)
	if err != nil {
		writeNotFound(writer)
		return
	}
	var selected *CheckpointSummary
	for index := range checkpoints {
		if checkpoints[index].ID == id {
			selected = &checkpoints[index]
			break
		}
	}
	if selected == nil {
		writeNotFound(writer)
		return
	}
	csrf, err := g.ensureCSRF(writer, request)
	if err != nil {
		writeNotFound(writer)
		return
	}
	idempotencyKey, err := g.newActionKey()
	if err != nil {
		writeNotFound(writer)
		return
	}
	writeHTMLHeaders(writer)
	_ = checkpointConfirmationTemplate.Execute(writer, checkpointConfirmationPage{
		ID: url.PathEscape(selected.ID), Name: selected.Name, CSRF: csrf, IdempotencyKey: idempotencyKey,
	})
}

func (g *Gateway) renderConfirmation(writer http.ResponseWriter, request *http.Request, identity Identity,
	id, action string) {
	environments, err := g.controlPlane.List(request.Context(), identity)
	if err != nil {
		writeNotFound(writer)
		return
	}
	var selected *EnvironmentSummary
	for index := range environments {
		if environments[index].ID == id {
			selected = &environments[index]
			break
		}
	}
	if selected == nil {
		writeNotFound(writer)
		return
	}
	csrf, err := g.ensureCSRF(writer, request)
	if err != nil {
		writeNotFound(writer)
		return
	}
	idempotencyKey, err := g.newActionKey()
	if err != nil {
		writeNotFound(writer)
		return
	}
	writeHTMLHeaders(writer)
	_ = confirmationTemplate.Execute(writer, confirmationPage{
		ID: url.PathEscape(selected.ID), Name: selected.Name, Host: selected.Host, Action: action, CSRF: csrf,
		IdempotencyKey: idempotencyKey,
	})
}

func (g *Gateway) runControlAction(request *http.Request, identity Identity, id, action string) error {
	idempotencyKey := request.Form.Get("idempotency")
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return ErrNotFound
	}
	switch action {
	case "start":
		return g.controlPlane.Start(request.Context(), identity, id, idempotencyKey)
	case "stop":
		return g.controlPlane.Stop(request.Context(), identity, id, idempotencyKey)
	case "archive":
		return g.controlPlane.Archive(request.Context(), identity, id, idempotencyKey)
	case "delete":
		return g.controlPlane.Delete(request.Context(), identity, id, idempotencyKey)
	case "extend":
		hours, err := strconv.Atoi(request.Form.Get("hours"))
		if err != nil || (hours != 1 && hours != 4) {
			return ErrNotFound
		}
		return g.controlPlane.Extend(request.Context(), identity, id, time.Duration(hours)*time.Hour,
			idempotencyKey)
	default:
		return ErrNotFound
	}
}

func (g *Gateway) newActionKey() (string, error) {
	value, err := randomBytes(g.random, 18)
	if err != nil {
		return "", err
	}
	return "ui_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func (g *Gateway) ensureCSRF(writer http.ResponseWriter, request *http.Request) (string, error) {
	if cookie, err := request.Cookie(csrfCookieName); err == nil {
		if decoded, decodeErr := base64.RawURLEncoding.DecodeString(cookie.Value); decodeErr == nil && len(decoded) == 32 {
			return cookie.Value, nil
		}
	}
	value, err := randomBytes(g.random, 32)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(value)
	http.SetCookie(writer, &http.Cookie{
		Name: csrfCookieName, Value: encoded, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60,
	})
	return encoded, nil
}

func (g *Gateway) validControlPOST(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodPost || request.Header.Get("Origin") != g.externalScheme+"://"+g.controlHost {
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	if request.ParseForm() != nil {
		return false
	}
	cookie, err := request.Cookie(csrfCookieName)
	return err == nil && secureEqual(cookie.Value, request.Form.Get("csrf"))
}

func parseControlPath(path, method string, query url.Values) (string, string, bool, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "env" {
		return "", "", false, false
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || !environmentIDPattern.MatchString(id) {
		return "", "", false, false
	}
	if parts[2] == "confirm" && method == http.MethodGet {
		action := query.Get("action")
		if action == "archive" || action == "delete" {
			return id, action, true, true
		}
		return "", "", false, false
	}
	if method != http.MethodPost {
		return "", "", false, false
	}
	switch parts[2] {
	case "start", "extend", "stop", "archive", "delete":
		return id, parts[2], false, true
	default:
		return "", "", false, false
	}
}

func parseCheckpointControlPath(path, method string) (string, bool, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "checkpoint" {
		return "", false, false
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || !environmentIDPattern.MatchString(id) {
		return "", false, false
	}
	if parts[2] == "confirm" && method == http.MethodGet {
		return id, true, true
	}
	if parts[2] == "delete" && method == http.MethodPost {
		return id, false, true
	}
	return "", false, false
}

func writeHTMLHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}
