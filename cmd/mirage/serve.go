package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/justinburger/mirage/internal/app"
	"github.com/justinburger/mirage/internal/config"
	"github.com/justinburger/mirage/internal/version"
)

// serveFlags are the settings of mirage serve after merging flags, the
// environment and the configuration file.
type serveFlags struct {
	admin, data, configPath, token, logLevel, logFormat string
	allowedHosts                                        []string
}

func runServe(ctx context.Context, args []string, s streams) int {
	var f serveFlags
	fs := newFlagSet("serve",
		"run the proxy services, the admin API and the web UI",
		[]string{"[--config FILE] [--admin ADDR] [--data FILE] [--admin-token TOKEN] [--allowed-host NAME]"},
		[]flagGroup{
			{"Flags", []string{"config", "admin", "data", "admin-token", "allowed-host", "log-level", "log-format"}},
		},
		[]string{
			"mirage serve                                   # UI on http://127.0.0.1:8484, data in ./mirage.db",
			"mirage serve --config examples/mirage.yaml     # also start the services defined in the file",
			"mirage serve --admin :8484 --admin-token s3cret  # reachable from the network, token required",
			"MIRAGE_ADMIN_TOKEN=s3cret mirage serve --admin 0.0.0.0:8484 --log-format json",
		}, s)
	fs.StringVar(&f.configPath, "config", "", "FILE|YAML or JSON file of recordings, profiles, services and fault rules, applied at start-up")
	fs.StringVar(&f.admin, "admin", "127.0.0.1:8484", "ADDR|listen address of the admin web UI, API and /metrics")
	fs.StringVar(&f.data, "data", "mirage.db", "FILE|SQLite database (created if missing)")
	fs.StringVar(&f.token, "admin-token", "", "TOKEN|require this bearer token for the admin API (default: $MIRAGE_ADMIN_TOKEN, then the config file)")
	fs.Func("allowed-host", "NAME|DNS name (optionally name:port) the admin API may be reached by when it has no admin token; repeat for several (IP addresses and localhost always work; default: server.allowed_hosts in the config file)", func(v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("must not be empty")
		}
		f.allowedHosts = append(f.allowedHosts, strings.TrimSpace(v))
		return nil
	})
	fs.StringVar(&f.logLevel, "log-level", "info", "LEVEL|debug, info, warn or error")
	fs.StringVar(&f.logFormat, "log-format", "text", "FORMAT|text or json")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}

	// The server section of the configuration file supplies defaults;
	// explicit flags win, then the environment, then the file.
	var cfg *config.Config
	if f.configPath != "" {
		var err error
		if cfg, err = config.Load(f.configPath); err != nil {
			fmt.Fprintf(s.err, "mirage serve: %v\n", err)
			return exitFail
		}
	}
	set := explicitFlags(fs)
	if cfg != nil {
		sv := cfg.Server
		pick := func(flagName string, dst *string, fromCfg string) {
			if !set[flagName] && fromCfg != "" {
				*dst = fromCfg
			}
		}
		pick("admin", &f.admin, sv.Admin)
		pick("data", &f.data, sv.Data)
		pick("log-level", &f.logLevel, sv.LogLevel)
		pick("log-format", &f.logFormat, sv.LogFormat)
		if !set["allowed-host"] {
			f.allowedHosts = sv.AllowedHosts
		}
		if !set["admin-token"] {
			if env := os.Getenv("MIRAGE_ADMIN_TOKEN"); env != "" {
				f.token = env
			} else {
				f.token = sv.AdminToken
			}
		}
	} else if !set["admin-token"] {
		f.token = os.Getenv("MIRAGE_ADMIN_TOKEN")
	}

	log, err := newLogger(f.logLevel, f.logFormat, s.err)
	if err != nil {
		return usageError(s, fs, "%v", err)
	}

	// SIGINT or SIGTERM starts a graceful shutdown; a second one kills the
	// process the usual way.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-sigCtx.Done():
		case <-finished:
			return
		}
		select {
		case <-finished:
			return
		default:
		}
		if ctx.Err() == nil { // a signal, not our caller giving up
			stop() // restore the default handlers for a second signal
			if strings.EqualFold(f.logFormat, "json") {
				log.Info("signal received, shutting down (send it again to force quit)")
			} else {
				fmt.Fprintln(s.err, "\nshutting down (press Ctrl-C again to force quit)")
			}
		}
	}()

	a, err := app.New(sigCtx, app.Options{
		Admin:        f.admin,
		Data:         f.data,
		ConfigPath:   f.configPath,
		Config:       cfg,
		AdminToken:   f.token,
		AllowedHosts: f.allowedHosts,
		Logger:       log,
	})
	if err != nil {
		fmt.Fprintf(s.err, "mirage serve: %v\n", err)
		return exitFail
	}

	if strings.EqualFold(f.logFormat, "json") {
		log.Info("mirage started", "version", version.String(), "admin_url", a.AdminURL(), "data", f.data, "config", f.configPath, "auth", f.token != "")
	} else {
		printBanner(s, a, f)
	}

	if err := a.Run(sigCtx); err != nil {
		fmt.Fprintf(s.err, "mirage serve: %v\n", err)
		return exitFail
	}
	return exitOK
}

// printBanner writes the short start-up summary.
func printBanner(s streams, a *app.App, f serveFlags) {
	auth := "none (anyone who can reach the admin address has full control)"
	if f.token != "" {
		auth = "admin token required"
	}
	fmt.Fprintf(s.err, "\nMirage %s\n", version.Version)
	fmt.Fprintf(s.err, "  UI        %s\n", a.AdminURL())
	fmt.Fprintf(s.err, "  data      %s\n", f.data)
	if f.configPath != "" {
		res := a.Applied()
		fmt.Fprintf(s.err, "  config    %s (%d created, %d updated, %d unchanged)\n", f.configPath, len(res.Created), len(res.Updated), len(res.Unchanged))
	}
	fmt.Fprintf(s.err, "  auth      %s\n", auth)
	fmt.Fprintf(s.err, "  metrics   %s/metrics\n\n", a.AdminURL())
}
