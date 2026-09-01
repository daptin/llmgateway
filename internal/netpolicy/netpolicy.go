package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

func DialContext(timeout time.Duration, allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if allowPrivate {
			return dialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("upstream address is invalid")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range addresses {
			if PrivateIP(candidate.IP) {
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			err = dialErr
		}
		if err != nil {
			return nil, err
		}
		return nil, errors.New("upstream resolved only to private-network addresses")
	}
}

// CloneTransport applies the same DNS and proxy policy to every module-owned
// HTTP client. Restricted clients connect directly so a proxy cannot resolve a
// public-looking hostname to a private address outside the guarded dial path.
func CloneTransport(base *http.Transport, timeout time.Duration, allowPrivate bool) *http.Transport {
	clone := base.Clone()
	clone.DialContext = DialContext(timeout, allowPrivate)
	if !allowPrivate {
		clone.Proxy = nil
	}
	return clone
}

func PrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return PrivateIP(ip)
	}
	return false
}

func PrivateIP(ip net.IP) bool {
	carrierGradeNAT := len(ip) > 0 && ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 0x40
	return !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || carrierGradeNAT
}

func RejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
