package main

// Subcommands that write the configuration file, for the installer (deploy/install.sh) and by
// hand:
//
//	mctunnel-relay init -config F -user NAME [-tcp 25500] [-tls 443] [-kcp 25500] [-players 25565-25664]
//	mctunnel-relay user list   -config F
//	mctunnel-relay user add    -config F NAME
//	mctunnel-relay user show   -config F NAME
//	mctunnel-relay user remove -config F NAME
//
// init creates a new file (never over an existing one). The user commands change only the
// "users" list: the rest of the file stays as it is, byte for byte. A change is checked the way
// the relay loads the file before the file is replaced, and the file keeps its owner and mode.
// A running relay picks up user changes with `systemctl reload mctunnel-relay` (SIGHUP).
// init, add and show print the user as name=, secret= and port= lines; list prints one
// "name<TAB>port<TAB>on|off" line per user and no secrets.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mctunnel/server/internal/config"
	"mctunnel/server/internal/protocol"
	"mctunnel/server/internal/relay"
)

const defaultConfigPath = "/etc/mctunnel/relay.json"

// runInit writes a new configuration with one user, who gets the first player port (25565 by
// default: friends then type the relay's address without a port).
func runInit(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("config", defaultConfigPath, "configuration file to create")
	name := fs.String("user", "", "the first user's name")
	tcp := fs.Int("tcp", 25500, "TCP port for hosts")
	tls := fs.Int("tls", 443, "TLS port for hosts (0: no TLS listener)")
	kcp := fs.Int("kcp", 25500, "UDP port for hosts over KCP (0: no KCP listener)")
	players := fs.String("players", "25565-25664", "players' TCP ports, min-max")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !protocol.ValidUser(*name) {
		fmt.Fprintf(errOut, "init: -user %q must be 1..%d characters of A-Z a-z 0-9 . _ -\n", *name, protocol.MaxUserLen)
		return 2
	}
	pmin, pmax, err := parseRange(*players)
	if err != nil {
		fmt.Fprintln(errOut, "init: -players:", err)
		return 2
	}
	if *tcp == 0 {
		fmt.Fprintln(errOut, "init: -tcp is required: the mod always offers TCP")
		return 2
	}
	u := config.User{Name: *name, Secret: genSecret(), Port: pmin}
	data := initialConfig(*tcp, *tls, *kcp, pmin, pmax, u)
	if _, err := config.Parse(data); err != nil {
		fmt.Fprintln(errOut, "init:", err)
		return 1
	}
	if err := createFile(*path, data); err != nil {
		fmt.Fprintln(errOut, "init:", err)
		return 1
	}
	printUser(out, u, u.Port)
	return 0
}

// initialConfig lays the file out like deploy/relay.example.json.
func initialConfig(tcp, tls, kcp, pmin, pmax int, u config.User) []byte {
	listen := []string{fmt.Sprintf(`{ "transport": "tcp", "address": ":%d" }`, tcp)}
	if tls != 0 {
		listen = append(listen, fmt.Sprintf(`{ "transport": "tls", "address": ":%d" }`, tls))
	}
	if kcp != 0 {
		listen = append(listen, fmt.Sprintf(`{ "transport": "kcp", "address": ":%d" }`, kcp))
	}
	var b strings.Builder
	b.WriteString("{\n  \"listen\": [\n    " + strings.Join(listen, ",\n    ") + "\n  ],\n")
	fmt.Fprintf(&b, "  \"public_bind\": \"\",\n  \"port_range\": { \"min\": %d, \"max\": %d },\n\n", pmin, pmax)
	b.WriteString(`  "max_hosts": 16,
  "max_players_per_host": 32,
  "max_players_per_ip": 8,
  "max_tunnels_per_user": 1,

  "handshake_timeout": "10s",
  "open_timeout": "10s",

  "max_handshakes": 256,
  "max_handshakes_per_ip": 32,
  "auth_failures": { "max": 5, "window": "10m", "ban": "15m" },

  "log_level": "info",

  "users": `)
	b.WriteString(formatUsers([]config.User{u}, "  "))
	b.WriteString("\n}\n")
	return []byte(b.String())
}

func parseRange(s string) (int, int, error) {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	pmin, err1 := strconv.Atoi(lo)
	pmax, err2 := strconv.Atoi(hi)
	if !ok || err1 != nil || err2 != nil || pmin < 1 || pmax > 65535 || pmin > pmax {
		return 0, 0, fmt.Errorf("%q is not a port range like 25565-25664", s)
	}
	return pmin, pmax, nil
}

// runUser lists, adds, shows or removes a user of an existing configuration.
func runUser(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: mctunnel-relay user list|add|show|remove -config FILE [NAME]")
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("config", defaultConfigPath, "configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	name := fs.Arg(0)
	switch cmd {
	case "list":
		if fs.NArg() > 0 {
			fmt.Fprintf(errOut, "user list: unexpected argument %q\n", name)
			return 2
		}
	case "add", "show", "remove":
		if fs.NArg() != 1 {
			fmt.Fprintf(errOut, "usage: mctunnel-relay user %s -config FILE NAME\n", cmd)
			return 2
		}
	default:
		fmt.Fprintf(errOut, "user: unknown command %q (list, add, show, remove)\n", cmd)
		return 2
	}

	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(errOut, "user:", err)
		return 1
	}
	cfg, err := config.Parse(data)
	if err != nil {
		fmt.Fprintf(errOut, "user: %s: %v\n", *path, err)
		return 1
	}
	homes := relay.HomePorts(users(cfg.Users), cfg.PortRange.Min, cfg.PortRange.Max)
	portOf := func(u config.User) int {
		if u.Port != 0 {
			return u.Port
		}
		return homes[u.Name]
	}
	index := -1
	for i, u := range cfg.Users {
		if u.Name == name {
			index = i
		}
	}

	switch cmd {
	case "list":
		for _, u := range cfg.Users {
			state := "on"
			if u.Disabled {
				state = "off"
			}
			fmt.Fprintf(out, "%s\t%d\t%s\n", u.Name, portOf(u), state)
		}
		return 0
	case "show":
		if index < 0 {
			fmt.Fprintf(errOut, "user show: no user %q in %s\n", name, *path)
			return 1
		}
		printUser(out, cfg.Users[index], portOf(cfg.Users[index]))
		return 0
	case "add":
		if !protocol.ValidUser(name) {
			fmt.Fprintf(errOut, "user add: %q must be 1..%d characters of A-Z a-z 0-9 . _ -\n", name, protocol.MaxUserLen)
			return 2
		}
		if index >= 0 {
			fmt.Fprintf(errOut, "user add: %q is already a user\n", name)
			return 1
		}
		port, err := freePort(cfg, homes)
		if err != nil {
			fmt.Fprintln(errOut, "user add:", err)
			return 1
		}
		u := config.User{Name: name, Secret: genSecret(), Port: port}
		if err := writeUsers(*path, data, append(cfg.Users, u)); err != nil {
			fmt.Fprintln(errOut, "user add:", err)
			return 1
		}
		printUser(out, u, port)
		return 0
	default: // remove
		if index < 0 {
			fmt.Fprintf(errOut, "user remove: no user %q in %s\n", name, *path)
			return 1
		}
		if len(cfg.Users) == 1 {
			fmt.Fprintf(errOut, "user remove: %q is the only user, and the relay needs one\n", name)
			return 1
		}
		rest := append(append([]config.User{}, cfg.Users[:index]...), cfg.Users[index+1:]...)
		if err := writeUsers(*path, data, rest); err != nil {
			fmt.Fprintln(errOut, "user remove:", err)
			return 1
		}
		return 0
	}
}

// freePort is the lowest port of the range that is neither a fixed port nor the home port of a
// user without one: taking it moves nobody's address (see relay.HomePorts).
func freePort(cfg *config.Config, homes map[string]int) (int, error) {
	used := make(map[int]bool)
	for _, u := range cfg.Users {
		if u.Port != 0 {
			used[u.Port] = true
		}
	}
	for _, p := range homes {
		used[p] = true
	}
	for p := cfg.PortRange.Min; p <= cfg.PortRange.Max; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port left in port_range %d..%d", cfg.PortRange.Min, cfg.PortRange.Max)
}

func printUser(out io.Writer, u config.User, port int) {
	fmt.Fprintf(out, "name=%s\nsecret=%s\nport=%d\n", u.Name, u.Secret, port)
}

// writeUsers replaces the "users" list of the file whose content is data, and nothing else.
func writeUsers(path string, data []byte, list []config.User) error {
	start, end, indent, err := usersSpan(data)
	if err != nil {
		return err
	}
	var next bytes.Buffer
	next.Write(data[:start])
	next.WriteString(formatUsers(list, indent))
	next.Write(data[end:])
	if _, err := config.Parse(next.Bytes()); err != nil {
		return fmt.Errorf("the result would not load, nothing was changed: %w", err)
	}
	return replaceFile(path, next.Bytes())
}

// usersSpan finds the "users" value of the top-level object: its byte range and the indentation
// of the line with its key.
func usersSpan(data []byte) (start, end int, indent string, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, 0, "", errors.New("the configuration is not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0, "", err
		}
		keyEnd := int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, "", err
		}
		if key, _ := tok.(string); key == "users" {
			end = int(dec.InputOffset())
			start = end - len(raw)
			line := data[bytes.LastIndexByte(data[:keyEnd], '\n')+1:]
			indent = string(line[:len(line)-len(bytes.TrimLeft(line, " \t"))])
			return start, end, indent, nil
		}
	}
	return 0, 0, "", errors.New("the configuration has no \"users\" list")
}

// formatUsers writes one user per line, as in deploy/relay.example.json.
func formatUsers(list []config.User, indent string) string {
	var b strings.Builder
	b.WriteString("[\n")
	for i, u := range list {
		name, _ := json.Marshal(u.Name)
		secret, _ := json.Marshal(u.Secret)
		fmt.Fprintf(&b, `%s  { "name": %s, "secret": %s`, indent, name, secret)
		if u.Port != 0 {
			fmt.Fprintf(&b, `, "port": %d`, u.Port)
		}
		if u.Disabled {
			b.WriteString(`, "disabled": true`)
		}
		b.WriteString(" }")
		if i < len(list)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(indent + "]")
	return b.String()
}

// createFile writes a new file readable by its owner only (it holds secrets); it never replaces
// an existing one.
func createFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// replaceFile swaps in the new content atomically, with the old file's mode and owner.
func replaceFile(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		return err
	}
	if err := copyOwner(tmp, fi); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}
