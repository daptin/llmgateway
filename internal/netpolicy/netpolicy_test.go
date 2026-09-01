package netpolicy

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRestrictedTransportCannotDelegateDNSPolicyToProxy(t *testing.T) {
	proxy := func(*http.Request) (*url.URL, error) {
		return url.Parse("http://proxy.example")
	}
	base := &http.Transport{Proxy: proxy}
	restricted := CloneTransport(base, 0, false)
	permitted := CloneTransport(base, 0, true)
	if restricted.Proxy != nil {
		t.Fatal("restricted transport retained a proxy DNS bypass")
	}
	if permitted.Proxy == nil || base.Proxy == nil {
		t.Fatal("explicit private-network transport did not preserve configured proxy behavior")
	}
}
