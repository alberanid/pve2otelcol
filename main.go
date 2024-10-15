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
	go func() {
		<-sigs
		p.Stop()
		done <- true
	}()

	/*
		logger, _ := ologgers.New(ologgers.OLoggerOptions{
			Endpoint:    "http://alloy.lan:4317",
			ServiceName: "lxc/666",
		})

		rnd := rand.New(rand.NewSource(time.Now().UnixNano())).Uint32()
		strJson := []byte(fmt.Sprintf("{\"message\": \"TEST %d\", \"int\": 42, \"null\": null, \"array\": [0, \"a\", 2, 3.14, null]}", rnd))
		var jData interface{}
		json.Unmarshal(strJson, &jData)
		logger.Log(jData)
	*/

	//p.RefreshLXCsMonitoring()
	<-done
}
