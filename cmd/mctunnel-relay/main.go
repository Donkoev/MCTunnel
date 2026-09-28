// Command mctunnel-relay is the VPS side of MCTunnel.
//
//	mctunnel-relay -config /etc/mctunnel/relay.json   run the relay
//	mctunnel-relay -config ... -check                 validate the configuration and exit
//	mctunnel-relay -config ... -print-ports           print the ports to open in a firewall and exit
//	mctunnel-relay -config ... -print-listen          print each host listener as "transport port" and exit
//	mctunnel-relay gen-secret                         print a new random secret for a user
//	mctunnel-relay init -config ... -user NAME        create a configuration with one user
//	mctunnel-relay user list|add|show|remove -config ... [NAME]
//	                                                  manage the users (see manage.go)
//	mctunnel-relay version
//
// SIGHUP reloads the user list (revoked users are disconnected at once); SIGINT/SIGTERM stop
// the relay after telling every host why.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mctunnel/server/internal/config"
	"mctunnel/server/internal/relay"
	"mctunnel/server/internal/transport"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gen-secret":
			fmt.Println(genSecret())
			return
		case "init":
			os.Exit(runInit(os.Args[2:], os.Stdout, os.Stderr))
		case "user", "users":
			os.Exit(runUser(os.Args[2:], os.Stdout, os.Stderr))
		case "version", "-version", "--version":
			fmt.Println("mctunnel-relay", version)
			return
		}
	}
	configPath := flag.String("config", "/etc/mctunnel/relay.json", "configuration file")
	check := flag.Bool("check", false, "validate the configuration and exit")
	printPorts := flag.Bool("print-ports", false, "print the ports to open in a firewall (TCP and UDP) and exit")
	printListen := flag.Bool("print-listen", false, "print each host listener as \"transport port\" and exit")
	flag.Parse()

	// Take over the signals before the slower part of startup. Until then a SIGHUP (reload)
	// would kill the process, and systemd does not restart a service that died of SIGHUP.
	// Signals that arrive early wait in the channel until the relay runs.
	var sig chan os.Signal
	if !*check && !*printPorts && !*printListen {
		sig = make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mctunnel-relay: %s: %v\n", *configPath, err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("%s: OK (%d users)\n", *configPath, len(cfg.Users))
		for _, w := range portWarnings(cfg.Users, cfg.PortRange) {
			fmt.Printf("%s: warning: %s\n", *configPath, w)
		}
		return
	}
	if *printListen {
		for _, l := range cfg.Listen {
			if _, port, err := net.SplitHostPort(l.Address); err == nil {
				fmt.Printf("%s %s\n", l.Transport, port)
			}
		}
		return
	}
	if *printPorts {
		for _, l := range cfg.Listen {
			if _, port, err := net.SplitHostPort(l.Address); err == nil {
				fmt.Printf("hosts %s/%s\n", port, transport.Network(l.Transport))
			}
		}
		fmt.Printf("players %d-%d\n", cfg.PortRange.Min, cfg.PortRange.Max)
		return
	}
	os.Exit(run(cfg, *configPath, sig))
}

func run(cfg *config.Config, configPath string, sig <-chan os.Signal) int {
	log := newLogger(cfg.LogLevel)
	srv := relay.New(options(cfg, log), users(cfg.Users))

	for _, lc := range cfg.Listen {
		ln, err := transport.Listen(lc)
		if err != nil {
			log.Error("cannot listen", "transport", lc.Transport, "address", lc.Address, "err", err)
			return 1
		}
		log.Info("listening for hosts", "transport", lc.Transport, "address", ln.Addr().String())
		go srv.Serve(ln)
	}
	log.Info("relay started", "version", version,
		"ports", fmt.Sprintf("%d-%d", cfg.PortRange.Min, cfg.PortRange.Max), "users", enabled(cfg.Users))
	logPortWarnings(log, cfg.Users, cfg.PortRange)

	for s := range sig {
		if s == syscall.SIGHUP {
			// Only the user list is reloaded; other settings need a restart.
			next, err := config.Load(configPath)
			if err != nil {
				log.Error("reload failed, keeping the current users", "err", err)
				continue
			}
			if changed := cfg.RestartOnly(next); len(changed) > 0 {
				log.Warn("changed settings are ignored until a restart", "settings", strings.Join(changed, ","))
			}
			// Fixed ports must fit the port range the relay runs with, not the one in the file.
			if err := config.ValidateUsers(next.Users, cfg.PortRange); err != nil {
				log.Error("reload failed, keeping the current users", "err", err)
				continue
			}
			srv.SetUsers(users(next.Users))
			log.Info("users reloaded", "users", enabled(next.Users))
			logPortWarnings(log, next.Users, cfg.PortRange)
			continue
		}
		log.Info("shutting down", "signal", s.String())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := srv.Shutdown(ctx)
		cancel()
		if err != nil {
			log.Warn("shutdown timed out", "err", err)
		}
		return 0
	}
	return 0
}

func options(cfg *config.Config, log *slog.Logger) relay.Options {
	return relay.Options{
		PublicBind:         cfg.PublicBind,
		PortMin:            cfg.PortRange.Min,
		PortMax:            cfg.PortRange.Max,
		MaxHosts:           cfg.MaxHosts,
		MaxPlayersPerHost:  cfg.MaxPlayersPerHost,
		MaxPlayersPerIP:    cfg.MaxPlayersPerIP,
		MaxTunnelsPerUser:  cfg.MaxTunnelsPerUser,
		HandshakeTimeout:   cfg.HandshakeTimeout.Duration,
		OpenTimeout:        cfg.OpenTimeout.Duration,
		MaxHandshakes:      cfg.MaxHandshakes,
		MaxHandshakesPerIP: cfg.MaxHandshakesPerIP,
		AuthMaxFailures:    cfg.AuthFailures.Max,
		AuthWindow:         cfg.AuthFailures.Window.Duration,
		AuthBan:            cfg.AuthFailures.Ban.Duration,
		Logger:             log,
	}
}

// users converts the configured users. Disabled users stay in the list: the relay does not let
// them log in (which revokes them), but keeps their ports, so that disabling one does not move
// another user's address.
func users(list []config.User) []relay.User {
	out := make([]relay.User, 0, len(list))
	for _, u := range list {
		out = append(out, relay.User{Name: u.Name, Secret: []byte(u.Secret), Port: u.Port, Disabled: u.Disabled})
	}
	return out
}

// enabled counts the users that may log in.
func enabled(list []config.User) int {
	n := 0
	for _, u := range list {
		if !u.Disabled {
			n++
		}
	}
	return n
}

// portWarnings lists the users without a fixed port that do not get the port derived from
// their name (it is another user's or a fixed port), with the port each of them gets instead.
// Warnings, not errors: those ports too stay the same across restarts, whoever connects first,
// but the operator should tell those users' friends (or give the users fixed ports).
func portWarnings(list []config.User, pr config.PortRange) []string {
	return relay.PortWarnings(users(list), pr.Min, pr.Max)
}

func logPortWarnings(log *slog.Logger, list []config.User, pr config.PortRange) {
	for _, w := range portWarnings(list, pr) {
		log.Warn("user port", "warning", w)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

// genSecret returns 256 random bits, base64url-encoded (43 characters).
func genSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintln(os.Stderr, "mctunnel-relay: crypto/rand:", err)
		os.Exit(1)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
