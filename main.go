package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/pve"
)

func main() {
	cfg := config.ParseArgs()
	stopCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	refreshSig := make(chan os.Signal, 1)
	signal.Notify(refreshSig, syscall.SIGUSR1)
	defer signal.Stop(refreshSig)

	p := pve.New(cfg)
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
			p.RefreshVMsMonitoring()
		}
	}
}
