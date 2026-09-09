package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"macbridge/internal/core"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type credential struct {
	Expires  int64  `json:"expires"`
	Client   string `json:"client"`
	Audience string `json:"audience"`
	Rotated  int64  `json:"rotated,omitempty"`
}
type oauthState struct {
	Epoch    string                `json:"epoch"`
	ClientID string                `json:"clientId"`
	Access   map[string]credential `json:"access"`
	Refresh  map[string]credential `json:"refresh"`
}
type approval struct {
	Client, Redirect, Challenge, State, Audience, Issuer string
	Expires                                              time.Time
}
type OAuth struct {
	mu              sync.Mutex
	state           oauthState
	file            string
	bearer, secret  string
	codes, requests map[string]approval
	public          string
	redirects       []string
}

func digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func equalDigest(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(digest(a)), []byte(digest(b))) == 1
}
func NewOAuth(cfg core.Config, token string) (*OAuth, error) {
	o := &OAuth{file: filepath.Join(cfg.DataDir, "oauth-go.json"), bearer: token, secret: os.Getenv("MAC_DEV_BRIDGE_OAUTH_CLIENT_SECRET"), codes: map[string]approval{}, requests: map[string]approval{}, public: strings.TrimRight(os.Getenv("MAC_DEV_BRIDGE_PUBLIC_URL"), "/"), redirects: strings.Split(os.Getenv("MAC_DEV_BRIDGE_OAUTH_REDIRECT_URIS"), ",")}
	if o.public != "" {
		u, e := url.Parse(o.public)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			o.public = ""
		}
	}
	b, _ := os.ReadFile(o.file)
	json.Unmarshal(b, &o.state)
	epoch := digest(token)
	if o.state.Epoch != epoch {
		o.state = oauthState{Epoch: epoch, ClientID: o.state.ClientID}
	}
	if configured := os.Getenv("MAC_DEV_BRIDGE_OAUTH_CLIENT_ID"); configured != "" {
		o.state.ClientID = configured
	}
	if o.state.ClientID == "" {
		o.state.ClientID = "mdb-" + core.ID()
	}
	if o.state.Access == nil {
		o.state.Access = map[string]credential{}
	}
	if o.state.Refresh == nil {
		o.state.Refresh = map[string]credential{}
	}
	o.prune()
	return o, core.AtomicJSON(o.file, o.state)
}
func (o *OAuth) ClientID() string { return o.state.ClientID }
func (o *OAuth) origin(r *http.Request) string {
	if o.public != "" {
		return o.public
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func (o *OAuth) validAudience(a, origin string) bool {
	if a == "" {
		return true
	}
	u, err := url.Parse(a)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(a, "#") {
		return false
	}
	base := strings.ToLower(strings.TrimRight(origin, "/"))
	if strings.HasSuffix(base, "/mcp") {
		base = strings.TrimSuffix(base, "/mcp")
	}
	normalized := strings.ToLower(u.Scheme+"://"+u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	return normalized == base || normalized == base+"/mcp"
}
func (o *OAuth) validRedirect(raw string) bool {
	if raw == "https://chatgpt.com/connector_platform_oauth_redirect" {
		return true
	}
	for _, v := range o.redirects {
		if strings.TrimSpace(v) != "" && raw == strings.TrimSpace(v) {
			return true
		}
	}
	return regexp.MustCompile(`^https://chatgpt\.com/connector/oauth/[A-Za-z0-9_-]{1,128}$`).MatchString(raw)
}
func (o *OAuth) Auth(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if equalDigest(token, o.bearer) {
		return "bearer"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.state.Access[digest(token)]
	if ok && c.Expires > time.Now().UnixMilli() && c.Client == o.state.ClientID && o.validAudience(c.Audience, o.origin(r)) {
		return "oauth"
	}
	return ""
}
func (o *OAuth) prune() {
	now := time.Now().UnixMilli()
	for _, m := range []map[string]credential{o.state.Access, o.state.Refresh} {
		for k, v := range m {
			if v.Expires <= now {
				delete(m, k)
			}
		}
		if len(m) > 256 {
			keys := []string{}
			for k := range m {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return m[keys[i]].Expires < m[keys[j]].Expires })
			for _, k := range keys[:len(keys)-256] {
				delete(m, k)
			}
		}
	}
	for _, m := range []map[string]approval{o.codes, o.requests} {
		for k, v := range m {
			if time.Now().After(v.Expires) {
				delete(m, k)
			}
		}
	}
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func oauthError(w http.ResponseWriter, status int, code, message string) {
	jsonResponse(w, status, map[string]any{"error": code, "error_description": message})
}
func (o *OAuth) Discovery(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp", "/mcp/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", "/mcp/.well-known/openid-configuration":
	default:
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return true
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		jsonResponse(w, 405, map[string]any{"error": "Method not allowed"})
		return true
	}
	origin := o.origin(r)
	if p == "/.well-known/oauth-protected-resource" || p == "/.well-known/oauth-protected-resource/mcp" {
		resource := origin
		if strings.HasSuffix(p, "/mcp") {
			resource += "/mcp"
		}
		jsonResponse(w, 200, map[string]any{"resource": resource, "authorization_servers": []string{origin}, "scopes_supported": []string{"mcp"}, "bearer_methods_supported": []string{"header"}, "resource_name": "MacBridge Go"})
		return true
	}
	switch p {
	case "/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp", "/mcp/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", "/mcp/.well-known/openid-configuration":
		jsonResponse(w, 200, map[string]any{"issuer": origin, "authorization_endpoint": origin + "/authorize", "token_endpoint": origin + "/token", "revocation_endpoint": origin + "/revoke", "revocation_endpoint_auth_methods_supported": []string{"none"}, "response_types_supported": []string{"code"}, "response_modes_supported": []string{"query"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"mcp"}, "authorization_response_iss_parameter_supported": true})
		return true
	}
	return false
}

var consent = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Connect MacBridge</title><style>body{font:16px system-ui;max-width:560px;margin:12vh auto;padding:24px;background:#f5f6f8;color:#17202a}input,button{padding:12px;font:inherit;width:100%;box-sizing:border-box;margin-top:12px}button{background:#173c64;color:white;border:0;border-radius:8px}code{overflow-wrap:anywhere}</style><h1>Connect to this Mac</h1><p>Authorize your MCP client to use the local tools.</p><p>Will redirect to: <code>{{.Redirect}}</code></p><p role="alert">{{.Error}}</p><form method="post" action="/authorize"><input type="hidden" name="rid" value="{{.RID}}"><label>Bridge token<input name="bearer" type="password" autocomplete="off" required></label><button type="submit">Authorize</button></form></html>`))

func (o *OAuth) consentPage(w http.ResponseWriter, status int, id string, a approval, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	formAction := "'self'"
	// Browsers apply form-action to the POST's redirect chain as well as its target.
	// The callback was already checked against the configured allowlist.
	if callback, err := url.Parse(a.Redirect); err == nil && callback.Scheme != "" {
		if (callback.Scheme == "http" || callback.Scheme == "https") && callback.Host != "" {
			formAction += " " + callback.Scheme + "://" + callback.Host
		} else {
			formAction += " " + callback.Scheme + ":"
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	consent.Execute(w, map[string]string{"RID": id, "Redirect": a.Redirect, "Error": message})
}
func (o *OAuth) Authorize(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prune()
	if r.Method == "GET" {
		q := r.URL.Query()
		a := approval{Client: q.Get("client_id"), Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), State: q.Get("state"), Audience: q.Get("resource"), Issuer: o.origin(r), Expires: time.Now().Add(5 * time.Minute)}
		if a.Client != o.state.ClientID {
			oauthError(w, 400, "invalid_client", "Unknown client_id")
			return
		}
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(a.Challenge) {
			oauthError(w, 400, "invalid_request", "Authorization code with S256 PKCE is required")
			return
		}
		if !o.validRedirect(a.Redirect) {
			oauthError(w, 400, "invalid_request", "Unrecognised redirect_uri")
			return
		}
		if !o.validAudience(a.Audience, a.Issuer) {
			oauthError(w, 400, "invalid_target", "resource does not identify this bridge")
			return
		}
		if len(o.requests) >= 32 {
			oauthError(w, 429, "temporarily_unavailable", "Too many pending approvals")
			return
		}
		id := core.ID()
		o.requests[id] = a
		o.consentPage(w, 200, id, a, "")
		return
	}
	if r.Method != "POST" {
		oauthError(w, 405, "invalid_request", "POST or GET required")
		return
	}
	if oauthForm(w, r) != nil {
		oauthError(w, 400, "invalid_request", "Invalid form")
		return
	}
	id := r.Form.Get("rid")
	a, ok := o.requests[id]
	delete(o.requests, id)
	if !ok || time.Now().After(a.Expires) {
		oauthError(w, 400, "invalid_request", "Approval expired")
		return
	}
	if !equalDigest(r.Form.Get("bearer"), o.bearer) {
		retry := core.ID()
		o.requests[retry] = a
		o.consentPage(w, 403, retry, a, "Token did not match. Try again.")
		return
	}
	code := core.ID() + core.ID()
	a.Expires = time.Now().Add(time.Minute)
	o.codes[digest(code)] = a
	u, _ := url.Parse(a.Redirect)
	q := u.Query()
	q.Set("code", code)
	q.Set("iss", a.Issuer)
	if a.State != "" {
		q.Set("state", a.State)
	}
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), 302)
}

// OAuth clients in the original bridge may send either a form or JSON object.
func oauthForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return r.ParseForm()
	}
	decoder := json.NewDecoder(r.Body)
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return err
	}
	if object == nil {
		return fmt.Errorf("JSON object required")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("Exactly one JSON object required")
	}
	r.Form = url.Values{}
	for key, value := range object {
		switch value.(type) {
		case string, bool, float64:
			r.Form.Set(key, fmt.Sprint(value))
		}
	}
	return nil
}
func decodeCredential(value string) string {
	if decoded, err := url.QueryUnescape(value); err == nil {
		return decoded
	}
	return value
}
func (o *OAuth) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		oauthError(w, 405, "invalid_request", "POST required")
		return
	}
	if oauthForm(w, r) != nil {
		oauthError(w, 400, "invalid_request", "Invalid form")
		return
	}
	client := r.Form.Get("client_id")
	secret := r.Form.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		if client == "" {
			client = decodeCredential(u)
		}
		if secret == "" {
			secret = decodeCredential(p)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prune()
	if client != o.state.ClientID || (o.secret != "" && secret != "" && !equalDigest(secret, o.secret)) {
		oauthError(w, 401, "invalid_client", "Client authentication failed")
		return
	}
	aud := r.Form.Get("resource")
	if !o.validAudience(aud, o.origin(r)) {
		oauthError(w, 400, "invalid_target", "Invalid resource")
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		key := digest(r.Form.Get("code"))
		a, ok := o.codes[key]
		delete(o.codes, key)
		if !ok || time.Now().After(a.Expires) || a.Client != client || r.Form.Has("redirect_uri") && a.Redirect != r.Form.Get("redirect_uri") {
			oauthError(w, 400, "invalid_grant", "Invalid or consumed code")
			return
		}
		verifier := r.Form.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if !regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`).MatchString(verifier) || base64.RawURLEncoding.EncodeToString(sum[:]) != a.Challenge {
			oauthError(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		if !o.validAudience(aud, a.Issuer) {
			oauthError(w, 400, "invalid_target", "Resource mismatch")
			return
		}
		aud = a.Issuer
	case "refresh_token":
		key := digest(r.Form.Get("refresh_token"))
		c, ok := o.state.Refresh[key]
		if !ok || c.Expires <= time.Now().UnixMilli() || c.Client != client || (c.Rotated != 0 && time.Now().UnixMilli()-c.Rotated > 120000) {
			oauthError(w, 400, "invalid_grant", "Refresh token expired or already rotated")
			return
		}
		if c.Audience != "" && !o.validAudience(aud, c.Audience) {
			oauthError(w, 400, "invalid_target", "Resource mismatch")
			return
		}
		if c.Rotated == 0 {
			c.Rotated = time.Now().UnixMilli()
			if c.Expires > c.Rotated+120000 {
				c.Expires = c.Rotated + 120000
			}
			o.state.Refresh[key] = c
		}
		aud = c.Audience
	default:
		oauthError(w, 400, "unsupported_grant_type", "Expected authorization_code or refresh_token")
		return
	}
	access, refresh := core.ID()+core.ID(), core.ID()+core.ID()
	o.state.Access[digest(access)] = credential{Expires: time.Now().Add(time.Hour).UnixMilli(), Client: client, Audience: aud}
	o.state.Refresh[digest(refresh)] = credential{Expires: time.Now().Add(30 * 24 * time.Hour).UnixMilli(), Client: client, Audience: aud}
	o.prune()
	if err := core.AtomicJSON(o.file, o.state); err != nil {
		oauthError(w, 500, "server_error", "Could not persist token state")
		return
	}
	jsonResponse(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "scope": "mcp"})
}
func (o *OAuth) Revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		oauthError(w, 405, "invalid_request", "POST required")
		return
	}
	if oauthForm(w, r) != nil {
		oauthError(w, 400, "invalid_request", "Invalid form")
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	key := digest(r.Form.Get("token"))
	delete(o.state.Access, key)
	delete(o.state.Refresh, key)
	if err := core.AtomicJSON(o.file, o.state); err != nil {
		oauthError(w, 500, "server_error", "Could not persist revocation")
		return
	}
	jsonResponse(w, 200, map[string]any{})
}
func (o *OAuth) RevokeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || o.Auth(r) != "bearer" {
		oauthError(w, 403, "access_denied", "Static bearer and POST required")
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	counts := map[string]int{"access": len(o.state.Access), "refresh": len(o.state.Refresh)}
	o.state.Access = map[string]credential{}
	o.state.Refresh = map[string]credential{}
	o.codes = map[string]approval{}
	o.requests = map[string]approval{}
	if err := core.AtomicJSON(o.file, o.state); err != nil {
		oauthError(w, 500, "server_error", "Could not persist revocation")
		return
	}
	jsonResponse(w, 200, map[string]any{"revoked": counts})
}
