package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/pve"
	"github.com/alberanid/pve2otelcol/version"
)

func main() {
	cfg, err := config.ParseArgs(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		config.PrintUsage(os.Stdout)
		return
	}
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		config.PrintUsage(os.Stderr)
		os.Exit(2)
	}
	if cfg.Version {
		fmt.Printf("version %s\n", version.VERSION)
		return
	}
	if cfg.Verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	stopCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	refreshSig := make(chan os.Signal, 1)
	signal.Notify(refreshSig, syscall.SIGUSR1)
	defer signal.Stop(refreshSig)

	p := pve.New(&cfg)
	if err := p.Start(); err != nil {
		slog.Error("unable to start monitoring", "error", err)
		os.Exit(1)
	}

	for {
		select {
		case <-stopCtx.Done():
			p.Stop()
			return
		case <-refreshSig:
			if err := p.RefreshVMsMonitoring(); err != nil {
				slog.Error("unable to refresh VM monitoring", "error", err)
			}
		}
	}
}
