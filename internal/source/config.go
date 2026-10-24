package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/netguard"
)

// NNTPConfig is the plaintext inside an nntp source's sealed_config,
// version 1. Unknown fields are rejected, so a misspelt field fails at
// PutSource instead of as an authentication error mid-download.
type NNTPConfig struct {
	V           int    `json:"v"`
	Host        string `json:"host"`
	Port        int    `json:"port,omitempty"`
	TLS         bool   `json:"tls"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Connections int    `json:"connections,omitempty"`
	Backup      bool   `json:"backup,omitempty"`
}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.?$`)

func parseNNTP(plain []byte) (usenet.Provider, error) {
	var c NNTPConfig

	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()

	// Decode errors can quote field names but never values; still, keep the
	// message generic so nothing from the plaintext reaches logs.
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return usenet.Provider{}, fmt.Errorf("%w: sealed config has an unknown field (%s)", ErrInvalid, strings.TrimPrefix(err.Error(), "json: "))
		}

		return usenet.Provider{}, fmt.Errorf("%w: sealed config is not a valid JSON object of the expected shape", ErrInvalid)
	}

	if dec.More() {
		return usenet.Provider{}, fmt.Errorf("%w: sealed config has data after the JSON object", ErrInvalid)
	}

	if c.V != 1 {
		return usenet.Provider{}, fmt.Errorf("%w: sealed config version %d, this agent supports 1", ErrInvalid, c.V)
	}

	host := strings.ToLower(strings.TrimSpace(c.Host))

	if err := checkHost(host); err != nil {
		return usenet.Provider{}, err
	}

	if !c.TLS {
		return usenet.Provider{}, fmt.Errorf("%w: tls must be true; web-app sources must use nntps", ErrInvalid)
	}

	port := c.Port
	if port == 0 {
		port = 563
	}

	if port < 1 || port > 65535 {
		return usenet.Provider{}, fmt.Errorf("%w: port must be 1-65535", ErrInvalid)
	}

	conns := c.Connections
	if conns == 0 {
		conns = 8
	}

	if conns < 1 || conns > 100 {
		return usenet.Provider{}, fmt.Errorf("%w: connections must be 1-100", ErrInvalid)
	}

	if (c.Username == "") != (c.Password == "") {
		return usenet.Provider{}, fmt.Errorf("%w: set both username and password, or neither", ErrInvalid)
	}

	if strings.ContainsAny(c.Username+c.Password, "\r\n") {
		return usenet.Provider{}, fmt.Errorf("%w: username and password must not contain line breaks", ErrInvalid)
	}

	return usenet.Provider{
		Host: strings.TrimSuffix(host, "."), Port: port, TLS: true,
		Username: c.Username, Password: c.Password,
		Connections: conns, Backup: c.Backup,
	}, nil
}

// checkHost accepts a public DNS name or IP address. Names are checked
// again after resolution, at dial time.
func checkHost(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("%w: host must be a DNS name or IP address", ErrInvalid)
	}

	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if netguard.IsLocal(ip.Unmap()) {
			return fmt.Errorf("%w: %s", ErrHostBlocked, ip)
		}

		return nil
	}

	if !hostPattern.MatchString(host) {
		return fmt.Errorf("%w: host must be a DNS name or IP address", ErrInvalid)
	}

	name := strings.TrimSuffix(host, ".")
	for _, local := range []string{"localhost", "local", "internal", "home.arpa", "lan"} {
		if name == local || strings.HasSuffix(name, "."+local) {
			return fmt.Errorf("%w: %s", ErrHostBlocked, name)
		}
	}

	return nil
}
