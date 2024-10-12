package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alberanid/pve2otelcol/ologgers"
	otellog "go.opentelemetry.io/otel/log"
)

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan bool, 1)
	go func() {
		sig := <-sigs
		fmt.Println()
		fmt.Println(sig)
		done <- true
	}()

	logger, _ := ologgers.New(ologgers.OLoggerOptions{
		Endpoint:    "http://alloy.lan:4317",
		ServiceName: "lxc/666",
	})
	olog := otellog.Record{}
	//strJson := []byte("{\"message\": \"TEST\", \"value\": 42}")
	//var jData map[string]interface{}
	//json.Unmarshal(strJson, &jData)
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	//olog.SetBody(otellog.StringValue(fmt.Sprintf("test log nr. %d", rnd.Uint32())))
	rkv := otellog.MapValue(
		otellog.KeyValue{
			Key:   "message",
			Value: otellog.StringValue(fmt.Sprintf("random test log nr. %d", rnd.Uint32())),
		},
		otellog.KeyValue{
			Key:   "value",
			Value: otellog.IntValue(42),
		},
	)
	olog.SetBody(rkv)

	//olog.SetBody(otellog.MapValue(jData))
	logger.LogRecord(olog)

	<-done
}
