// Command mediasyncd is the mediasync media session bridge service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/daemon"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "0.1.0-dev"

func main() {
	os.Exit(run())
}

func run() int {
	paths := config.DefaultPaths()
	configPath := flag.String("config", paths.ConfigFile, "configuration file")
	checkOnly := flag.Bool("check-config", false, "validate the configuration and exit")
	logLevel := flag.String("log-level", "", "override general.log_level (debug, info, warn, error)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mediasyncd", version)
		return 0
	}
	paths.ConfigFile = *configPath
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediasyncd:", err)
		return 2
	}
	if *logLevel != "" {
		cfg.General.LogLevel = *logLevel
		if err := cfg.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "mediasyncd:", err)
			return 2
		}
	}
	if *checkOnly {
		fmt.Printf("configuration OK (%s)\n", paths.ConfigFile)
		return 0
	}
	paths = cfg.ResolvePaths(paths)

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.General.LogLevel))
	hopts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, hopts)
	if cfg.General.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, hopts)
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	svc, err := daemon.New(daemon.Options{Paths: paths, Config: cfg, Logger: log, Version: version})
	if err != nil {
		log.Error("cannot initialise", "err", err)
		return 1
	}
	if err := svc.Start(); err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	// The socket is listening: tell systemd we are ready (Type=notify), so
	// `systemctl restart` only returns once clients can connect.
	sdNotify("READY=1")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			sdNotify("RELOADING=1")
			err := svc.Reload()
			sdNotify("READY=1")
			if err != nil {
				log.Error("reload failed; keeping previous configuration", "err", err)
			}
		}
	}()

	svc.Run(ctx)
	sdNotify("STOPPING=1")
	svc.Close()
	return 0
}
