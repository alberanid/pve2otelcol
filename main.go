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
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan bool, 1)

	p := pve.New(cfg)
	p.Start()

	go func() {
		<-sigs
		p.Stop()
		done <- true
	}()
	<-done
}
