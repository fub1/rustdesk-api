package httpapi

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/oidclogin"
	"github.com/lejianwen/rustdesk-api/v2/utils"
)

func boundaryHandler() *Handler {
	return &Handler{translate: func(_, message string) string { return message }}
}

func TestUnconfiguredOIDCRoutesFailClosed(t *testing.T) {
	h := boundaryHandler()
	for name, handler := range map[string]gin.HandlerFunc{
		"client begin": h.ClientOIDCBegin, "admin begin": h.AdminOIDCBegin, "client poll": h.ClientOIDCPoll,
		"admin poll": h.AdminOIDCPoll, "callback": h.OIDCCallback, "bind": h.OIDCBind, "info": h.OIDCInfo,
		"approve": h.OIDCConfirm, "bind-confirm": h.OIDCBindConfirm, "unbind": h.OIDCUnbind, "my-links": h.OIDCMyOauth,
	} {
		t.Run(name, func(t *testing.T) {
			g := gin.New()
			g.POST("/oidc", handler)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest("POST", "/oidc?state=private-state&code=private-code", strings.NewReader(`{"token":"private-token"}`)))
			if w.Code != 503 || strings.Contains(w.Body.String(), "private-") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unconfigured OIDC leaked or did not fail closed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestOIDCRejectsOversizedBodiesAndAmbiguousPollParameters(t *testing.T) {
	h := boundaryHandler()
	// Invalid input must be rejected before using any service dependency.
	h.oidc = &oidclogin.Service{}
	for _, tc := range []struct{ body, query string }{
		{`{"op":"` + strings.Repeat("x", 129) + `"}`, ""},
		{`{"op":"ours","padding":"` + strings.Repeat("x", maxOIDCBody) + `"}`, ""},
		{`{"op":"ours"}{"op":"other"}`, ""},
		{"", "code=private-one&code=private-two"},
		{"", "code=private-code&id=one&id=two"},
		{"", "code=private-code&uuid=" + strings.Repeat("x", 257)},
	} {
		g := gin.New()
		g.POST("/begin", h.ClientOIDCBegin)
		g.GET("/poll", h.ClientOIDCPoll)
		method, path := "POST", "/begin"
		if tc.query != "" {
			method, path = "GET", "/poll?"+tc.query
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(tc.body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "ParamsError") || strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("unsafe input was not rejected: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCallbackRejectsDuplicateAndOversizedCredentialsWithoutReflection(t *testing.T) {
	h := boundaryHandler()
	h.oidc = &oidclogin.Service{}
	for _, query := range []string{
		"state=private-state&state=another-state&code=private-code",
		"state=private-state&code=private-one&code=private-two",
		"state=private-state&error=private-error&error=other-error",
		"state=private-state&code=private-code&error=private-error",
		"state=private-state&code=private-code&iss=",
		"state=" + strings.Repeat("x", 257) + "&code=private-code",
		"state=private-state&code=" + strings.Repeat("x", 4097),
	} {
		g := gin.New()
		g.SetHTMLTemplate(template.Must(template.New("oauth_fail.html").Parse(`<html>{{.message}}</html>`)))
		g.GET("/callback", h.OIDCCallback)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "/callback?"+query, nil))
		if w.Code != 200 || w.Body.String() != "<html>OauthFailed</html>" || strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("callback input reflected or accepted: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("missing callback browser protections: %v", w.Header())
		}
	}
}

func TestWebApprovalRequiresNonemptyNondefaultPassword(t *testing.T) {
	for _, password := range []string{"", utils.DefaultInitPassword, "a-personal-password"} {
		hash, err := utils.EncryptPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		want := password != "" && password != utils.DefaultInitPassword
		if approvalPasswordAllowed(hash) != want || approvalPasswordAllowed(utils.Md5(password+"rustdesk-api")) != want {
			t.Fatalf("incorrect web-approval password policy for fixture %q", password)
		}
	}
	if approvalPasswordAllowed("") || approvalPasswordAllowed("invalid-password-hash") {
		t.Fatal("empty or invalid password hash permitted web approval")
	}
}
