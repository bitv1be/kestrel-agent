package netx

import (
	"net/http"
	"net/url"
	"testing"
)

func TestProxyPrecedenceAndEnv(t *testing.T) {
	if got := (Chain{Global: "global", Component: "none", Provider: "socks5://host:1080"}).Resolve(); got != "socks5://host:1080" {
		t.Fatal(got)
	}
	env := map[string]string{"HTTPS_PROXY": "socks5://alice:password@proxy:1080", "NO_PROXY": "local"}
	factory := NewFactory(Config{Env: func(k string) string { return env[k] }})
	defer factory.CloseIdle()
	req := func(host string) *http.Request {
		r, _ := http.NewRequest("GET", "https://"+host, nil)
		return r
	}
	tr, err := factory.Transport("")
	if err != nil {
		t.Fatal(err)
	}
	p, err := tr.Proxy(req("remote.example"))
	if err != nil || p == nil || p.Scheme != "socks5" {
		t.Fatalf("env proxy = %v, %v", p, err)
	}
	if p, err = tr.Proxy(req("local")); err != nil || p != nil {
		t.Fatalf("NO_PROXY = %v, %v", p, err)
	}
	tr, err = factory.Transport("none")
	if err != nil || tr.Proxy != nil {
		t.Fatalf("none proxy = %v, %v", tr, err)
	}
	tr, err = factory.Transport("http://user:pass@override:8080")
	if err != nil {
		t.Fatal(err)
	}
	p, err = tr.Proxy(req("local"))
	if err != nil || p == nil || p.Host != "override:8080" {
		t.Fatalf("override = %v, %v", p, err)
	}
	if got := Redact((&url.URL{Scheme: "socks5", User: url.UserPassword("user", "password"), Host: "host:1080"}).String()); got == "" || got == "socks5://user:password@host:1080" {
		t.Fatal(got)
	}
}
