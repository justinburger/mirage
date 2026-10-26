// Package app wires every Mirage component together: it opens the store,
// applies the configuration file, builds the proxy, replay and API managers,
// and runs the admin HTTP server. cmd/mirage serve is a thin shell around it,
// and the integration tests use it directly.
//
// Start-up order (see docs/ARCHITECTURE.md): New opens the store, applies the
// configuration (config.Apply), loads the fault rules (ops.LoadFaults) and
// builds the managers; Run then starts the metrics registry, runs
// replay.Manager.Recover, applies every enabled service (proxy.Manager.Apply)
// and finally starts api.Server.Run and the admin HTTP server. Shutdown is the
// reverse: admin HTTP server, replay jobs, proxy listeners (draining the
// capture queue), the registry, the store.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/api"
	"github.com/justinburger/mirage/internal/config"
	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/netutil"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/proxy"
	"github.com/justinburger/mirage/internal/replay"
	"github.com/justinburger/mirage/internal/store"
	"github.com/justinburger/mirage/internal/version"
	"github.com/justinburger/mirage/internal/virtual"
	"github.com/justinburger/mirage/web"
)

// Compile-time checks that the concrete components fit the interfaces that
// connect them.
var (
	_ api.ServiceController = (*proxy.Manager)(nil)
	_ api.ReplayController  = (*replay.Manager)(nil)
	_ proxy.Virtualizer     = (*virtual.Engine)(nil)
)

// ShutdownTimeout bounds the whole graceful shutdown (draining requests and
// captures, saving replay jobs, closing the store).
const ShutdownTimeout = 20 * time.Second

// Options configures an App.
type Options struct {
	// Admin is the listen address of the admin UI, API and /metrics
	// ("127.0.0.1:8484"). Port 0 picks a free port (see AdminAddr).
	Admin string
	// Data is the SQLite database path (":memory:" for a private in-memory
	// database).
	Data string
	// ConfigPath is an optional configuration file, applied at start-up.
	ConfigPath string
	// Config is an already loaded configuration, used instead of reading
	// ConfigPath (the CLI loads the file once to read its server section).
	Config *config.Config
	// AdminToken, when not empty, is required by the admin API.
	AdminToken string
	// Logger defaults to a discard logger.
	Logger *slog.Logger
}

// App is a running (or ready to run) Mirage instance.
type App struct {
	opts Options
	log  *slog.Logger

	store    *store.SQLite
	registry *metrics.Registry
	faults   *fault.Injector
	proxy    *proxy.Manager
	replay   *replay.Manager
	api      *api.Server
	httpSrv  *http.Server
	newConns *netutil.NewConns
	ln       net.Listener

	// applied describes what the configuration file changed.
	applied config.ApplyResult

	runOnce      sync.Once
	shutdownOnce sync.Once
	shutdownErr  error
	regStop      context.CancelFunc
	regDone      chan struct{}
}

// New builds an App: it opens the store, applies the configuration file,
// creates the managers and binds the admin listener. Nothing is served until
// Run. On error everything already opened is closed again.
func New(ctx context.Context, opts Options) (*App, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Admin == "" {
		opts.Admin = "127.0.0.1:8484"
	}
	if opts.Data == "" {
		opts.Data = "mirage.db"
	}

	a := &App{opts: opts, log: log}
	ok := false
	defer func() {
		if !ok {
			a.closeQuiet()
		}
	}()

	st, err := store.Open(opts.Data)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", opts.Data, err)
	}
	a.store = st
	log.Info("database opened", "path", opts.Data)

	// Configuration file.
	cfg := opts.Config
	if cfg == nil && opts.ConfigPath != "" {
		if cfg, err = config.Load(opts.ConfigPath); err != nil {
			return nil, err
		}
	}
	if cfg != nil {
		if err := a.applyConfig(ctx, cfg); err != nil {
			return nil, err
		}
	}

	// Fault injection rules.
	a.faults = fault.NewInjector()
	if err := ops.LoadFaults(ctx, st, a.faults); err != nil {
		return nil, fmt.Errorf("load fault rules: %w", err)
	}

	a.registry = metrics.NewRegistry()

	a.proxy = proxy.NewManager(proxy.Deps{
		Store:        st,
		Metrics:      a.registry,
		Faults:       a.faults,
		Virtualizers: a.virtualizer,
		Logger:       log.With("component", "proxy"),
	})

	a.replay = replay.NewManager(replay.Deps{
		Store:   st,
		Metrics: a.registry,
		Logger:  log.With("component", "replay"),
		OnChange: func(j model.ReplayJob) {
			log.Info("replay job", "id", j.ID, "name", j.Name, "status", string(j.Status), "error", j.Error)
		},
	})

	a.api = api.New(api.Deps{
		Store:      st,
		Services:   a.proxy,
		Replays:    a.replay,
		Metrics:    a.registry,
		Faults:     a.faults,
		UI:         web.FS(),
		AdminToken: opts.AdminToken,
		Version:    version.Version,
		DataPath:   opts.Data,
		Logger:     log.With("component", "api"),
	})

	// Bind the admin address now so that "address in use" is reported before
	// any service starts, and so AdminAddr is known to the caller.
	ln, err := net.Listen("tcp", opts.Admin)
	if err != nil {
		return nil, fmt.Errorf("admin listener: %w", err)
	}
	a.ln = ln
	a.httpSrv = &http.Server{
		Handler:           a.api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.With("component", "admin").Handler(), slog.LevelWarn),
	}
	a.newConns = netutil.Track(a.httpSrv)
	if opts.AdminToken == "" && !isLoopback(ln.Addr()) {
		log.Warn("admin API is reachable from the network without an admin token; set --admin-token", "addr", ln.Addr().String())
	}
	ok = true
	return a, nil
}

// applyConfig validates and applies the configuration file to the store.
func (a *App) applyConfig(ctx context.Context, cfg *config.Config) error {
	// config cannot import anonymize, so selectors are checked here.
	var problems []string
	for _, p := range cfg.Profiles {
		p.Rules = append([]model.AnonymizationRule(nil), p.Rules...) // WithDefaults edits rules in place
		if err := anonymize.Validate(p.WithDefaults()); err != nil {
			problems = append(problems, fmt.Sprintf("profile %q: %v", p.Name, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %w", &model.ValidationError{Problems: problems})
	}
	res, err := cfg.Apply(ctx, a.store)
	a.applied = res
	for _, s := range res.Created {
		a.log.Info("config: created", "object", s)
	}
	for _, s := range res.Updated {
		a.log.Info("config: updated", "object", s)
	}
	if len(res.Unchanged) > 0 {
		a.log.Debug("config: unchanged", "objects", strings.Join(res.Unchanged, ", "))
	}
	if err != nil {
		return fmt.Errorf("apply config: %w", err)
	}
	return nil
}

// virtualizer is the proxy.VirtualizerFactory. It returns an untyped nil
// interface on error: returning the nil *virtual.Engine directly would give
// the proxy a non-nil interface wrapping a nil pointer.
func (a *App) virtualizer(ctx context.Context, svc model.Service, n *pathnorm.Normalizer) (proxy.Virtualizer, error) {
	e, err := virtual.Build(ctx, a.store, virtual.OptionsFromService(svc, n))
	if err != nil {
		return nil, err
	}
	return e, nil
}

// AdminAddr is the resolved address of the admin listener.
func (a *App) AdminAddr() string { return a.ln.Addr().String() }

// AdminURL is the URL to open the UI at ("http://127.0.0.1:8484").
func (a *App) AdminURL() string {
	host, port, err := net.SplitHostPort(a.AdminAddr())
	if err != nil {
		return "http://" + a.AdminAddr()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Store exposes the store (tests, embedding).
func (a *App) Store() store.Store { return a.store }

// Applied reports what the configuration file created and updated.
func (a *App) Applied() config.ApplyResult { return a.applied }

// Run starts the registry ticker, every enabled service and the admin server,
// then blocks until ctx is done (or the admin server fails), shuts everything
// down and returns. It must be called at most once.
func (a *App) Run(ctx context.Context) error {
	first := false
	a.runOnce.Do(func() { first = true })
	if !first {
		return errors.New("app: Run called twice")
	}

	// The registry ticks until the very end, so replay jobs and capture
	// workers shutting down still have their last second closed.
	regCtx, regStop := context.WithCancel(context.WithoutCancel(ctx))
	a.regStop = regStop
	a.regDone = make(chan struct{})
	go func() {
		defer close(a.regDone)
		a.registry.Run(regCtx)
	}()

	// Jobs a previous process left running cannot be resumed.
	if err := a.replay.Recover(ctx); err != nil {
		a.log.Warn("recovering replay jobs", "error", err)
	}

	a.startServices(ctx)

	// The API's broadcaster runs on the app's context: cancelling it closes
	// every SSE stream, which http.Server.Shutdown would otherwise wait for.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.api.Run(runCtx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpSrv.Serve(a.ln) }()
	a.log.Info("admin server listening", "addr", a.AdminAddr(), "url", a.AdminURL())

	var err error
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("admin server: %w", err)
		}
	}
	cancel()
	if serr := a.shutdown(); err == nil {
		err = serr
	}
	return err
}

// startServices applies every enabled stored service. A service that fails to
// start is logged and skipped; the others still run.
func (a *App) startServices(ctx context.Context) {
	svcs, err := a.store.ListServices(ctx)
	if err != nil {
		a.log.Error("listing services", "error", err)
		return
	}
	started := 0
	for _, svc := range svcs {
		if !svc.Enabled {
			a.log.Debug("service disabled", "name", svc.Name)
			continue
		}
		if err := a.proxy.Apply(ctx, svc); err != nil {
			a.log.Error("service failed to start", "name", svc.Name, "listen", svc.ListenAddr, "mode", string(svc.Mode), "error", err)
			continue
		}
		st := a.proxy.Status(svc.ID)
		a.log.Info("service started", "name", svc.Name, "listen", st.Addr, "mode", string(svc.Mode), "upstream", svc.Upstream)
		started++
	}
	a.log.Info("services started", "running", started, "total", len(svcs))
}

// Close shuts the app down if Run has not (or is still running). It is safe
// to call more than once and after Run returned.
func (a *App) Close() error { return a.shutdown() }

// shutdown stops everything in reverse start order, bounded by
// ShutdownTimeout. Only the first call does the work.
func (a *App) shutdown() error {
	a.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		a.shutdownErr = a.stop(ctx)
	})
	return a.shutdownErr
}

func (a *App) stop(ctx context.Context) error {
	var errs []error
	a.log.Info("shutting down")
	began := time.Now()
	phase := func(name string) {
		a.log.Debug("shutdown phase done", "phase", name, "elapsed", time.Since(began).Round(time.Millisecond).String())
	}

	// 1. Admin HTTP server. Open SSE streams were closed when the API's
	// context ended; whatever is left is cut when the deadline passes.
	if a.httpSrv != nil {
		a.newConns.CloseAll() // idle browser connections must not delay Shutdown by 5 s
		if err := a.httpSrv.Shutdown(ctx); err != nil {
			_ = a.httpSrv.Close()
			if !errors.Is(err, http.ErrServerClosed) {
				errs = append(errs, fmt.Errorf("admin server: %w", err))
			}
		}
	}
	if a.ln != nil {
		_ = a.ln.Close()
	}
	if a.api != nil {
		a.api.WaitContext(ctx)
	}
	phase("admin server")

	// 2. Replay jobs: stopped and saved with their partial results.
	if a.replay != nil {
		if err := a.replay.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("replay: %w", err))
		}
	}
	phase("replay jobs")

	// 3. Service listeners, then the capture queue.
	if a.proxy != nil {
		if err := a.proxy.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("proxy: %w", err))
		}
	}
	phase("service listeners and capture queue")

	// 4. The registry.
	if a.regStop != nil {
		a.regStop()
		<-a.regDone
	}

	// 5. The store.
	if a.store != nil {
		if err := a.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: %w", err))
		}
	}
	if len(errs) == 0 {
		a.log.Info("shutdown complete")
	}
	return errors.Join(errs...)
}

// closeQuiet releases what New had opened when it fails half way.
func (a *App) closeQuiet() {
	if a.ln != nil {
		_ = a.ln.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.replay != nil {
		_ = a.replay.Shutdown(ctx)
	}
	if a.proxy != nil {
		_ = a.proxy.Shutdown(ctx)
	}
	if a.store != nil {
		_ = a.store.Close()
	}
}

// isLoopback reports whether addr is a loopback TCP address.
func isLoopback(addr net.Addr) bool {
	ta, ok := addr.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

var _ io.Closer = (*App)(nil)
