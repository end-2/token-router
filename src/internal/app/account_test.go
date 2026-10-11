package app

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"token-router/src/internal/bucket"
	"token-router/src/internal/config"
)

func accountJWT(payload string) string {
	return "Bearer eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func accountBasic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func TestAccountIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name, authorization, claim, want string
	}{
		{"basic", accountBasic("alice", "secret:password"), "sub", "alice"},
		{"whitespace", " \t" + strings.ReplaceAll(accountBasic("alice", "pw"), " ", "\t\t") + "\u2003", "sub", "alice"},
		{"extra field", accountBasic("alice", "pw") + " extra", "sub", ""},
		{"empty password", accountBasic("alice", ""), "sub", "alice"},
		{"case insensitive", strings.ToLower("Basic") + " " + base64.StdEncoding.EncodeToString([]byte("alice:pw")), "sub", "alice"},
		{"bearer", accountJWT(`{"sub":"alice"}`), "sub", "alice"},
		{"empty signature", strings.TrimSuffix(accountJWT(`{"sub":"alice"}`), "signature"), "sub", "alice"},
		{"extra JWT segment", accountJWT(`{"sub":"alice"}`) + ".extra", "sub", ""},
		{"empty JWT header", "Bearer ." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"alice"}`)) + ".sig", "sub", ""},
		{"empty JWT payload", "Bearer header..sig", "sub", ""},
		{"custom claim", accountJWT(`{"sub":"alice","account_id":"team"}`), "account_id", "team"},
		{"missing", "", "sub", ""},
		{"unsupported", "Digest anything", "sub", ""},
		{"empty user", accountBasic("", "pw"), "sub", ""},
		{"control user", accountBasic("alice\n", "pw"), "sub", ""},
		{"missing colon", "Basic YWxpY2U=", "sub", ""},
		{"invalid base64", "Basic !!!", "sub", ""},
		{"opaque bearer", "Bearer opaque-token", "sub", ""},
		{"invalid JWT payload", "Bearer header.!!!.sig", "sub", ""},
		{"missing claim", accountJWT(`{"id":"alice"}`), "sub", ""},
		{"numeric claim", accountJWT(`{"sub":123}`), "sub", ""},
		{"null claim", accountJWT(`{"sub":null}`), "sub", ""},
		{"object claim", accountJWT(`{"sub":{"id":"alice"}}`), "sub", ""},
		{"empty claim", accountJWT(`{"sub":" "}`), "sub", ""},
		{"invalid JSON", accountJWT(`{`), "sub", ""},
		{"long identifier", accountBasic(strings.Repeat("a", 1025), "pw"), "sub", ""},
		{"long auth", "Bearer " + strings.Repeat("a", 16<<10), "sub", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			r.Header.Set("Authorization", tc.authorization)
			got, ok := accountIdentifier(r, config.Account{Enabled: true, JWTClaim: tc.claim})
			if ok != (tc.want != "") || (ok && got != tc.want) {
				t.Fatalf("identifier=%q ok=%v, want %q", got, ok, tc.want)
			}
		})
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Add("Authorization", accountBasic("alice", "pw"))
	r.Header.Add("Authorization", accountBasic("bob", "pw"))
	if _, ok := accountIdentifier(r, config.Account{JWTClaim: "sub"}); ok {
		t.Fatal("ambiguous Authorization headers accepted")
	}
}

func TestAccountLimitsAndPolicyUpdate(t *testing.T) {
	cfg := config.Defaults()
	cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst = .001, 1
	cfg.RateLimit.Account.Enabled = true
	limiter, err := bucket.New(t.Context(), cfg.RateLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := limiter.Close(); err != nil {
			t.Error(err)
		}
	})
	a := testApplication(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), limiter, func(c *config.Config) { c.RateLimit = cfg.RateLimit })
	request := func(auth string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	request("", 401)
	request("Bearer opaque-token", 401)
	request(accountBasic("alice", "password-one"), 204)
	request(accountBasic("alice", "password-two"), 429)
	request(accountJWT(`{"sub":"alice"}`), 429)
	request(accountJWT(`{"sub":"bob"}`), 204)
	request(accountJWT(`{"sub":"bob","iat":123}`), 429)
	update := func(account config.Account) {
		t.Helper()
		next := a.snapshot()
		next.RateLimit.Account = account
		if err := a.updatePolicy(next, false); err != nil {
			t.Fatal(err)
		}
	}
	update(config.Account{Enabled: true, JWTClaim: "account_id"})
	request(accountJWT(`{"sub":"carol"}`), 401)
	request(accountJWT(`{"account_id":"carol"}`), 204)
	update(config.Account{Enabled: false, JWTClaim: "sub"})
	request("", 204)
	request(accountBasic("new-user", "pw"), 429)
	update(config.Account{Enabled: true, JWTClaim: "sub"})
	request(accountBasic("alice", "pw"), 429)
}

func TestAccountUsageRetainsIdentityAcrossPolicyUpdate(t *testing.T) {
	cfg := config.Defaults()
	cfg.RateLimit.Account.Enabled = true
	cfg.RateLimit.TokensPerSecond, cfg.RateLimit.TokenBurst = .001, 1
	limiter, err := bucket.New(t.Context(), cfg.RateLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := limiter.Close(); err != nil {
			t.Error(err)
		}
	})
	entered, release := make(chan struct{}), make(chan struct{})
	a := testApplication(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"usage":{"total_tokens":5}}`); err != nil {
			t.Error(err)
		}
	}), limiter, func(c *config.Config) { c.RateLimit = cfg.RateLimit })
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Authorization", accountJWT(`{"sub":"alice"}`))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.handler().ServeHTTP(w, r)
	}()
	<-entered
	next := a.snapshot()
	next.RateLimit.Account.JWTClaim = "account_id"
	if err := a.updatePolicy(next, false); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	<-done
	if w.Code != 200 {
		t.Fatalf("usage response status %d", w.Code)
	}
	for _, tc := range []struct {
		id      string
		allowed bool
	}{{"alice", false}, {"bob", true}} {
		decision, err := limiter.Allow(bucket.WithAccount(t.Context(), tc.id))
		if err != nil || decision.Allowed != tc.allowed {
			t.Fatalf("%s: %+v, %v", tc.id, decision, err)
		}
	}
}
