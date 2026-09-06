package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/term"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/application"
	"xpanel/internal/config"
	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/web"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/worker"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: xpanel <serve|admin>")
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stderr)
	case "admin":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: xpanel admin <init|reset-password>")
			return 2
		}
		return runAdmin(args[1], args[2:], stdin, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "unknown command")
		return 2
	}
}

func configFlag(name string, args []string, stderr io.Writer) (*flag.FlagSet, *string, *bool, *string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to XPanel JSON configuration")
	passwordStdin := flags.Bool("password-stdin", false, "read a single password from stdin")
	username := flags.String("username", "", "administrator username (init only)")
	if err := flags.Parse(args); err != nil {
		return nil, nil, nil, nil, err
	}
	if *path == "" {
		return nil, nil, nil, nil, errors.New("--config PATH is required")
	}
	return flags, path, passwordStdin, username, nil
}

func bootstrap(configPath string) (config.Config, *sqlite.Store, *security.Keyring, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	root, err := security.LoadRootKey(cfg.Security.RootKeyFile)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	keyring, err := security.NewKeyring(root)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	db, err := sqlite.Open(context.Background(), cfg.Storage.DatabasePath, cfg.Storage.BusyTimeout.Duration)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	if err := sqlite.Migrate(context.Background(), db.Write); err != nil {
		db.Close()
		return config.Config{}, nil, nil, err
	}
	store := sqlite.NewStore(db)
	verifier, nonce, err := keyring.NewVerifier()
	if err != nil {
		store.Close()
		return config.Config{}, nil, nil, err
	}
	if err := store.EnsureSingletons(context.Background(), cfg.Initial.QuotaTimezone, cfg.Xray.APIEndpoint,
		cfg.Xray.SupportedVersion, verifier, nonce, time.Now().UTC()); err != nil {
		store.Close()
		return config.Config{}, nil, nil, err
	}
	if err := store.VerifyKey(context.Background(), keyring.Verify); err != nil {
		store.Close()
		return config.Config{}, nil, nil, err
	}
	return cfg, store, keyring, nil
}

func runServe(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to XPanel JSON configuration")
	if err := flags.Parse(args); err != nil || *configPath == "" {
		if err == nil {
			fmt.Fprintln(stderr, "--config PATH is required")
		}
		return 2
	}
	cfg, store, keyring, err := bootstrap(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "startup failed:", err)
		return 3
	}
	defer store.Close()
	logger := logging.New(stderr, parseLogLevel(cfg.Logging.Level))
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}
	auth, err := application.NewAuthService(store, ports.SystemClock{})
	if err != nil {
		logger.Error("initialize authentication", "error_kind", "internal")
		return 3
	}
	sessions := scs.New()
	webmiddleware.ConfigureSessions(sessions, sqlite.NewSessionStore(store.DB(), cfg.Security.SessionIdleTimeout.Duration,
		cfg.Security.SessionAbsoluteTimeout.Duration), cfg.Security.SessionIdleTimeout.Duration,
		cfg.Security.SessionAbsoluteTimeout.Duration, !cfg.Server.InsecureDevelopment)
	target := ports.InstanceTarget{APIEndpoint: cfg.Xray.APIEndpoint, ExpectedVersion: cfg.Xray.SupportedVersion, RPCTimeout: cfg.Xray.RPCTimeout.Duration}
	xrayClient, err := xrayadapter.New(target)
	if err != nil {
		logger.Error("initialize Xray API client", logging.FieldErrorKind, "invalid_argument")
		return 3
	}
	defer xrayClient.Close()
	instance, err := store.ManagedInstance(context.Background())
	if err != nil {
		logger.Error("load managed instance", logging.FieldErrorKind, "internal")
		return 3
	}
	target.InstanceID = instance.ID
	clock := ports.SystemClock{}
	node := &sync.Mutex{}
	var validator *worker.ProfileValidator
	requestValidation := func(id domain.ID) {
		if validator != nil {
			validator.Enqueue(id)
		}
	}
	profiles := application.NewProfileService(store, xrayClient, keyring, clock, target, requestValidation)
	synchronizer := worker.NewSynchronizer(store, xrayClient, keyring, clock, logger, node, worker.SynchronizerOptions{
		MaxRetryInterval: cfg.Workers.MaxRetryInterval.Duration, RPCTimeout: cfg.Xray.RPCTimeout.Duration, OnProfileRecovered: requestValidation})
	users := application.NewUserService(store, keyring, clock, synchronizer.Wake)
	connections := application.NewConnectionService(store, keyring)
	settings := application.NewSettingsService(store).WithClock(clock)
	dashboard := application.NewDashboardService(store, clock, cfg.Workers.TrafficInterval.Duration, cfg.Workers.ReconcileInterval.Duration)
	auditService := application.NewAuditService(store)
	validator = worker.NewProfileValidator(profiles, store, logger, node, cfg.Workers.ReconcileInterval.Duration)
	traffic := application.NewTrafficService(store, xrayClient, clock, target, cfg.Workers.TrafficInterval.Duration, synchronizer.Wake, logger)
	quota := application.NewQuotaService(store, clock, synchronizer.Wake, logger)
	collector := worker.NewCollector(traffic, cfg.Workers.TrafficInterval.Duration, logger)
	scheduler := worker.NewScheduler(quota, clock, time.Minute, logger)
	reconciliation := application.NewReconciliationService(store, xrayClient, clock, target, node, cfg.Workers.ReconcileInterval.Duration,
		synchronizer.Wake, profiles.RunValidation, logger)
	reconciler := worker.NewReconciler(reconciliation, cfg.Workers.ReconcileInterval.Duration, logger)

	server, err := web.NewServer(cfg, web.RouteDependencies{Auth: auth, Profiles: profiles, Users: users, Connections: connections,
		Settings: settings, Dashboard: dashboard, Audit: auditService, Sessions: sessions, CSRFKey: keyring.CSRFKey(), Secure: !cfg.Server.InsecureDevelopment, Logger: logger})
	if err != nil {
		logger.Error("initialize HTTP server", "error_kind", "internal")
		return 3
	}

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	var workers sync.WaitGroup
	// 启动顺序：synchronizer → reconciler → validator → collector → scheduler，再开放 readiness（plan §Startup and shutdown order）。
	for _, run := range []func(context.Context){synchronizer.Run, reconciler.Run, validator.Run, collector.Run, scheduler.Run} {
		workers.Add(1)
		go func(run func(context.Context)) { defer workers.Done(); run(workerCtx) }(run)
	}
	server.SetReady(true)
	logger.Info("XPanel ready", logging.FieldComponent, "server", logging.FieldNodeID, instance.ID.String(),
		"xray_endpoint", cfg.Xray.APIEndpoint, "supported_version", cfg.Xray.SupportedVersion, "listen", cfg.Server.Listen)

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(shutdown)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration)
		defer cancel()
		// 优雅关闭：先停止接收请求，再取消后台 RPC，等待 worker 完成短事务并释放租约。
		_ = server.Shutdown(ctx)
		stopWorkers()
		workers.Wait()
	}()
	if err := server.ListenAndServe(); err != nil {
		logger.Error("HTTP server stopped", logging.FieldErrorKind, "internal")
		return 4
	}
	workers.Wait()
	return 0
}

func runAdmin(command string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_, configPath, passwordStdin, username, err := configFlag("admin "+command, args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if command != "init" && command != "reset-password" {
		fmt.Fprintln(stderr, "unknown admin command")
		return 2
	}
	if command == "reset-password" && *username != "" {
		fmt.Fprintln(stderr, "--username is only valid with admin init")
		return 2
	}
	if command == "init" && *username == "" {
		if *passwordStdin {
			fmt.Fprintln(stderr, "--username is required with --password-stdin")
			return 2
		}
		fmt.Fprint(stderr, "Username: ")
		line, readErr := bufio.NewReader(stdin).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			fmt.Fprintln(stderr, "unable to read username")
			return 2
		}
		*username = strings.TrimSpace(line)
	}
	password, err := readPassword(stdin, stderr, *passwordStdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	_, store, _, err := bootstrap(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration or storage failed:", err)
		return 3
	}
	defer store.Close()
	auth, err := application.NewAuthService(store, ports.SystemClock{})
	if err != nil {
		fmt.Fprintln(stderr, "authentication service unavailable")
		return 3
	}
	if command == "init" {
		if _, err := auth.InitializeAdministrator(context.Background(), *username, password); err != nil {
			fmt.Fprintln(stderr, err)
			return 4
		}
		fmt.Fprintln(stdout, "administrator initialized")
		return 0
	}
	if err := auth.ResetPassword(context.Background(), password); err != nil {
		fmt.Fprintln(stderr, err)
		return 4
	}
	fmt.Fprintln(stdout, "administrator password reset")
	return 0
}

func readPassword(stdin io.Reader, stderr io.Writer, fromStdin bool) ([]byte, error) {
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, 258))
		if err != nil {
			return nil, errors.New("unable to read password")
		}
		value := strings.TrimSuffix(string(data), "\n")
		value = strings.TrimSuffix(value, "\r")
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("password input must contain exactly one non-empty line")
		}
		return []byte(value), nil
	}
	file, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil, errors.New("interactive password input requires a terminal; use --password-stdin")
	}
	fmt.Fprint(stderr, "Password: ")
	first, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return nil, errors.New("unable to read password")
	}
	fmt.Fprint(stderr, "Confirm password: ")
	second, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(stderr)
	if err != nil || string(first) != string(second) {
		return nil, errors.New("password confirmation does not match")
	}
	return first, nil
}

func parseLogLevel(value string) slog.Leveler {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
