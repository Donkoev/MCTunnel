// Package config loads and validates the relay configuration (JSON).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mctunnel/server/internal/protocol"
	"mctunnel/server/internal/transport"
)

// MinSecretLen is the minimum secret length. Anyone who records an authentication (nonces and
// HMAC travel in clear) can brute-force a weak secret offline, so secrets must be random
// (`mctunnel-relay gen-secret`), never human passwords.
const MinSecretLen = 32

// Config is the relay configuration file.
type Config struct {
	// Endpoints for host connections (control and data share them).
	Listen []transport.Config `json:"listen"`
	// Address to bind the players' external ports to; "" = all interfaces.
	PublicBind string `json:"public_bind"`
	// External ports handed out to tunnels.
	PortRange PortRange `json:"port_range"`

	MaxHosts          int `json:"max_hosts"`
	MaxPlayersPerHost int `json:"max_players_per_host"`
	// Concurrent players of one tunnel from one address (IPv6: one /64), so a single client
	// cannot fill the tunnel; default max(4, max_players_per_host/4). Raise it when many players
	// share an address: friends behind one NAT or a carrier-grade NAT, or a proxy in front of
	// the relay.
	MaxPlayersPerIP int `json:"max_players_per_ip"`
	// When a user opens more tunnels than this, the oldest one is replaced.
	MaxTunnelsPerUser int `json:"max_tunnels_per_user"`

	// Time for a new host connection to authenticate.
	HandshakeTimeout Duration `json:"handshake_timeout"`
	// Time for the host to answer OPEN with a data connection before the player is dropped.
	OpenTimeout Duration `json:"open_timeout"`

	MaxHandshakes      int `json:"max_handshakes"`
	MaxHandshakesPerIP int `json:"max_handshakes_per_ip"`

	AuthFailures AuthFailures `json:"auth_failures"`

	// debug, info, warn, error
	LogLevel string `json:"log_level"`

	Users []User `json:"users"`
}

type PortRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// AuthFailures: Max failed authentications from one address (IPv6: one /64) within Window
// get it banned for Ban.
type AuthFailures struct {
	Max    int      `json:"max"`
	Window Duration `json:"window"`
	Ban    Duration `json:"ban"`
}

// User is one host allowed to open tunnels. Remove it, set Disabled or change Secret and reload
// (SIGHUP) to revoke access; its open tunnel is closed immediately.
type User struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	// Optional fixed external port (inside port_range) so friends can keep the same address.
	Port     int  `json:"port,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
}

// Duration is a time.Duration written as a Go duration string ("10s", "15m").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %s", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes, defaults and validates configuration JSON. Unknown fields are rejected to catch
// typos in field names.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if len(c.Listen) == 0 {
		c.Listen = []transport.Config{{Transport: "tcp", Address: ":25500"}}
	}
	for i := range c.Listen {
		if c.Listen[i].Transport == "" {
			c.Listen[i].Transport = "tcp"
		}
	}
	if c.PortRange == (PortRange{}) {
		c.PortRange = PortRange{Min: 25600, Max: 25699}
	}
	setInt(&c.MaxHosts, 16)
	setInt(&c.MaxPlayersPerHost, 32)
	setInt(&c.MaxPlayersPerIP, max(4, c.MaxPlayersPerHost/4))
	setInt(&c.MaxTunnelsPerUser, 1)
	setDur(&c.HandshakeTimeout, 10*time.Second)
	setDur(&c.OpenTimeout, 10*time.Second)
	setInt(&c.MaxHandshakes, 256)
	setInt(&c.MaxHandshakesPerIP, 32)
	setInt(&c.AuthFailures.Max, 5)
	setDur(&c.AuthFailures.Window, 10*time.Minute)
	setDur(&c.AuthFailures.Ban, 15*time.Minute)
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

func setInt(v *int, def int) {
	if *v == 0 {
		*v = def
	}
}

func setDur(v *Duration, def time.Duration) {
	if v.Duration == 0 {
		v.Duration = def
	}
}

// Validate checks the whole configuration and reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	pr := c.PortRange
	if pr.Min < 1 || pr.Max > 65535 || pr.Min > pr.Max {
		fail("port_range: need 1 <= min <= max <= 65535, got %d..%d", pr.Min, pr.Max)
	}
	for i, l := range c.Listen {
		if _, err := transport.Get(l.Transport); err != nil {
			fail("listen[%d]: %v", i, err)
		}
		if (l.Cert == "") != (l.Key == "") {
			fail("listen[%d]: set both cert and key, or neither (a self-signed certificate is generated then)", i)
		}
		_, port, err := net.SplitHostPort(l.Address)
		if err != nil {
			fail("listen[%d]: address %q: %v", i, l.Address, err)
			continue
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			fail("listen[%d]: bad port in %q", i, l.Address)
		} else if p >= pr.Min && p <= pr.Max {
			fail("listen[%d]: port %d is inside port_range %d..%d", i, p, pr.Min, pr.Max)
		}
	}
	if c.PublicBind != "" && net.ParseIP(c.PublicBind) == nil {
		fail("public_bind: %q is not an IP address", c.PublicBind)
	}
	for name, v := range map[string]int{
		"max_hosts": c.MaxHosts, "max_players_per_host": c.MaxPlayersPerHost,
		"max_players_per_ip": c.MaxPlayersPerIP, "max_tunnels_per_user": c.MaxTunnelsPerUser,
		"max_handshakes": c.MaxHandshakes, "max_handshakes_per_ip": c.MaxHandshakesPerIP,
		"auth_failures.max": c.AuthFailures.Max,
	} {
		if v < 1 {
			fail("%s must be >= 1", name)
		}
	}
	for name, d := range map[string]Duration{
		"handshake_timeout": c.HandshakeTimeout, "open_timeout": c.OpenTimeout,
		"auth_failures.window": c.AuthFailures.Window, "auth_failures.ban": c.AuthFailures.Ban,
	} {
		if d.Duration <= 0 {
			fail("%s must be positive", name)
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("log_level: %q is not one of debug, info, warn, error", c.LogLevel)
	}
	if err := ValidateUsers(c.Users, pr); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RestartOnly lists, by their JSON names, the settings that differ between c and next. A reload
// (SIGHUP) applies only the users, so these changes wait for a restart.
func (c *Config) RestartOnly(next *Config) []string {
	var changed []string
	cur, nv := reflect.ValueOf(*c), reflect.ValueOf(*next)
	for i := 0; i < cur.NumField(); i++ {
		name, _, _ := strings.Cut(cur.Type().Field(i).Tag.Get("json"), ",")
		if name == "users" {
			continue
		}
		if !reflect.DeepEqual(cur.Field(i).Interface(), nv.Field(i).Interface()) {
			changed = append(changed, name)
		}
	}
	return changed
}

// strippedBySomeClient matches what the clients trim off a secret: Java's String.trim (every
// character up to U+0020, control characters included) and Go's strings.TrimSpace (Unicode
// spaces).
func strippedBySomeClient(r rune) bool { return r <= ' ' || unicode.IsSpace(r) }

// ValidateUsers checks the user list (also used when reloading).
func ValidateUsers(users []User, pr PortRange) error {
	var errs []error
	if len(users) == 0 {
		errs = append(errs, errors.New("users: at least one user is required"))
	}
	names := map[string]bool{}
	ports := map[int]string{}
	for i, u := range users {
		if !protocol.ValidUser(u.Name) {
			errs = append(errs, fmt.Errorf("users[%d]: name %q must be 1..%d characters of A-Z a-z 0-9 . _ -", i, u.Name, protocol.MaxUserLen))
		}
		if names[u.Name] {
			errs = append(errs, fmt.Errorf("users[%d]: duplicate name %q", i, u.Name))
		}
		names[u.Name] = true
		if len(u.Secret) < MinSecretLen {
			errs = append(errs, fmt.Errorf("users[%d] (%s): secret must be at least %d characters; generate one with `mctunnel-relay gen-secret`", i, u.Name, MinSecretLen))
		}
		// The clients strip whitespace around the secret they send, so such a secret could never
		// match: every login would fail and look like a wrong secret.
		if strings.TrimFunc(u.Secret, strippedBySomeClient) != u.Secret || strings.IndexFunc(u.Secret, unicode.IsControl) >= 0 {
			errs = append(errs, fmt.Errorf("users[%d] (%s): secret starts or ends with whitespace or contains control characters; clients strip whitespace, so no host could log in (remove the spaces inside the quotes)", i, u.Name))
		}
		if u.Port != 0 {
			if u.Port < pr.Min || u.Port > pr.Max {
				errs = append(errs, fmt.Errorf("users[%d] (%s): port %d is outside port_range %d..%d", i, u.Name, u.Port, pr.Min, pr.Max))
			}
			if other, dup := ports[u.Port]; dup {
				errs = append(errs, fmt.Errorf("users[%d] (%s): port %d is already assigned to %s", i, u.Name, u.Port, other))
			}
			ports[u.Port] = u.Name
		}
	}
	return errors.Join(errs...)
}
