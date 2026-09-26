package paths

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// HFMirrorURL routes only canonical Hugging Face URLs through an opt-in mirror.
// Saved URLs remain canonical, so changing mirrors never changes identity.
func HFMirrorURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Host, "huggingface.co") {
		return raw, nil
	}
	mirror := strings.TrimSpace(os.Getenv("HF_ENDPOINT"))
	if mirror == "" {
		return raw, nil
	}
	endpoint, err := url.Parse(mirror)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && isLoopbackHost(endpoint.Hostname()))) {
		return "", fmt.Errorf("invalid HF_ENDPOINT: use an HTTPS origin (HTTP is allowed only for loopback)")
	}
	u.Scheme, u.Host = endpoint.Scheme, endpoint.Host
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}
