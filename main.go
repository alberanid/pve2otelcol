package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/pve"
)

func main() {
	cfg := config.ParseArgs()
	done := make(chan bool, 1)
	stopSigs := make(chan os.Signal, 1)
	signal.Notify(stopSigs, syscall.SIGINT, syscall.SIGTERM)
	refreshSig := make(chan os.Signal, 1)
	signal.Notify(refreshSig, syscall.SIGUSR1)

	p := pve.New(cfg)
	p.Start()

	go func() {
		<-stopSigs
		p.Stop()
		done <- true
	}()
	go func() {
		for {
			<-refreshSig
			p.RefreshVMsMonitoring()
		}
	}()
	<-done
}
