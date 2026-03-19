package tool

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
)

func ValidateURLForTool(toolName, rawURL string) error {
	switch toolName {
	case "SimpleHttp":
		return validateHTTPURL(rawURL)
	case "Transmission":
		u, err := url.Parse(rawURL)
		if err != nil {
			return errors.Wrap(err, "invalid url")
		}
		if strings.EqualFold(u.Scheme, "magnet") {
			return nil
		}
		return validateParsedHTTPURL(u)
	default:
		return nil
	}
}

func validateHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.Wrap(err, "invalid url")
	}
	return validateParsedHTTPURL(u)
}

func validateParsedHTTPURL(u *url.URL) error {
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return errors.New("only http/https urls are supported")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("invalid url host")
	}
	if isUnsafeHostName(host) {
		return fmt.Errorf("forbidden host: %s", host)
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if isUnsafeIP(ip) {
			return fmt.Errorf("forbidden target ip: %s", ip.String())
		}
		return nil
	}
	ipList, err := net.LookupIP(host)
	if err != nil {
		return errors.Wrap(err, "failed to resolve host")
	}
	if len(ipList) == 0 {
		return errors.New("resolved host has no ip")
	}
	for _, resolvedIP := range ipList {
		if isUnsafeIP(resolvedIP) {
			return fmt.Errorf("forbidden resolved ip: %s", resolvedIP.String())
		}
	}
	return nil
}

func isUnsafeHostName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(host), ".localhost")
}

func isUnsafeIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if utils.IsLocalIP(ip) {
		return true
	}
	if ip4 := ip.To4(); ip4 == nil {
		return ip.IsPrivate()
	}
	return ip.IsPrivate()
}
