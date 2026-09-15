package geoip

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOnlineCountry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if got := r.URL.Path; got != "/json/81.2.69.198" {
			t.Errorf("request path = %q, want /json/81.2.69.198", got)
		}
		_, _ = w.Write([]byte(`{"countryCode":"jp"}`)) // lowercase on purpose
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/json/{ip}?fields=countryCode", time.Second, 16)
	code, ok := o.Country("81.2.69.198")
	if !ok || code != "JP" {
		t.Fatalf("Country = (%q, %v), want (JP, true)", code, ok)
	}
	// second call for the same IP is served from the cache
	if code, ok := o.Country("81.2.69.198"); !ok || code != "JP" {
		t.Fatalf("cached Country = (%q, %v), want (JP, true)", code, ok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("endpoint called %d times for the same IP, want 1", got)
	}
}

func TestOnlineSkipsHTTPForInvalidAndLocalIPs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("endpoint must not be called")
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 16)
	for _, ip := range []string{"not-an-ip", "", "127.0.0.1", "10.0.0.5", "192.168.1.1", "::1", "fe80::1", "224.0.0.1"} {
		if _, ok := o.Country(ip); ok {
			t.Errorf("Country(%q) reported a hit, want miss", ip)
		}
	}
}

func TestOnlineFailureReturnsMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 16)
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected miss on HTTP 500")
	}
	// the miss itself is cached: the next call doesn't retry
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected cached miss")
	}
}

func TestOnlineGarbageResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"reserved range"}`)) // no countryCode
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 16)
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected miss when the response carries no countryCode")
	}
}

func TestOnlineTimeoutReturnsMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"countryCode":"JP"}`))
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", 30*time.Millisecond, 16)
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected miss when the endpoint exceeds the timeout")
	}
}

func TestOnlineEmptyCountryCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"countryCode":""}`))
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 16)
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected miss for an empty countryCode")
	}
}

func TestOnlineRejectsNonISOCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"countryCode":"toolong"}`))
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 16)
	if _, ok := o.Country("81.2.69.198"); ok {
		t.Fatal("expected miss for a value that is not an ISO 3166-1 alpha-2 code")
	}
}

func TestLRUEvictsOldest(t *testing.T) {
	c := newLRU(2)
	c.put("a", "AA")
	c.put("b", "BB")
	if code, ok := c.get("a"); !ok || code != "AA" {
		t.Fatalf("get(a) = (%q, %v), want (AA, true)", code, ok)
	}
	c.put("c", "CC") // evicts b (a was just refreshed)
	if _, ok := c.get("b"); ok {
		t.Fatal("expected b to be evicted")
	}
	if code, ok := c.get("a"); !ok || code != "AA" {
		t.Fatalf("get(a) = (%q, %v), want (AA, true)", code, ok)
	}
	if code, ok := c.get("c"); !ok || code != "CC" {
		t.Fatalf("get(c) = (%q, %v), want (CC, true)", code, ok)
	}
	c.put("a", "ZZ") // existing key updates in place
	if code, _ := c.get("a"); code != "ZZ" {
		t.Fatalf("get(a) = %q, want ZZ", code)
	}
}

func TestLRUCacheHitDoesNotRequery(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"countryCode":"DE"}`))
	}))
	defer srv.Close()

	o := NewOnline(srv.URL+"/{ip}", time.Second, 4096)
	for i := 0; i < 5; i++ {
		if code, ok := o.Country("203.0.113.7"); !ok || code != "DE" {
			t.Fatalf("call %d: Country = (%q, %v), want (DE, true)", i+1, code, ok)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("endpoint called %d times across 5 lookups, want 1", got)
	}
}

type stubResolver struct {
	code  string
	ok    bool
	calls int
}

func (s *stubResolver) Country(ip string) (string, bool) {
	s.calls++
	return s.code, s.ok
}

func TestChainOrder(t *testing.T) {
	primary := &stubResolver{code: "GB", ok: true}
	fallback := &stubResolver{code: "US", ok: true}
	c := Chain(primary, fallback)
	if code, ok := c.Country("1.2.3.4"); !ok || code != "GB" {
		t.Fatalf("Country = (%q, %v), want (GB, true)", code, ok)
	}
	if fallback.calls != 0 {
		t.Fatal("fallback consulted although the primary hit")
	}

	primary = &stubResolver{} // always misses
	fallback = &stubResolver{code: "US", ok: true}
	c = Chain(primary, nil, fallback) // nil entries are skipped
	if code, ok := c.Country("1.2.3.4"); !ok || code != "US" {
		t.Fatalf("Country = (%q, %v), want (US, true)", code, ok)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("calls: primary=%d fallback=%d, want 1 and 1", primary.calls, fallback.calls)
	}

	if _, ok := Chain(&stubResolver{}).Country("1.2.3.4"); ok {
		t.Fatal("expected miss when every resolver misses")
	}
}

func TestNewComposesResolvers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.mmdb")
	if c, ok := New(missing, false).(*chain); !ok || len(c.resolvers) != 1 {
		t.Fatal("online disabled: expected the MMDB resolver only")
	}
	if c, ok := New(missing, true).(*chain); !ok || len(c.resolvers) != 2 {
		t.Fatal("online enabled: expected MMDB plus online fallback")
	}
}
