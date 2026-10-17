// Command mockmodel serves scripted model responses for tests (design §14).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/usr-lex/swarmageddon/internal/mockmodel"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
)

func main() {
	listen := flag.String("listen", ":9999", "address to listen on")
	script := flag.String("script", "", "YAML or JSON script of turns (empty: always reply \"ok\")")
	flag.Parse()
	flush, terr := telemetry.Init("mockmodel")
	log := slog.New(telemetry.LogHandler(slog.NewTextHandler(os.Stderr, nil)))
	if terr != nil {
		log.Warn("error reporting is off", "err", terr)
	}
	var sc *mockmodel.Script
	if *script != "" {
		var err error
		if sc, err = mockmodel.LoadScript(*script); err != nil {
			log.Error("loading script", "err", err)
			flush()
			os.Exit(1)
		}
	}
	s := mockmodel.New(sc)
	s.Logger = log
	log.Info("mockmodel listening", "addr", *listen)
	if err := http.ListenAndServe(*listen, telemetry.Handler(s)); err != nil {
		log.Error("serve", "err", err)
		flush()
		os.Exit(1)
	}
}
