// Command mockmodel serves scripted model responses for tests (design §14).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/usr-lex/swarmageddon/internal/mockmodel"
)

func main() {
	listen := flag.String("listen", ":9999", "address to listen on")
	script := flag.String("script", "", "YAML or JSON script of turns (empty: always reply \"ok\")")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var sc *mockmodel.Script
	if *script != "" {
		var err error
		if sc, err = mockmodel.LoadScript(*script); err != nil {
			log.Error("loading script", "err", err)
			os.Exit(1)
		}
	}
	s := mockmodel.New(sc)
	s.Logger = log
	log.Info("mockmodel listening", "addr", *listen)
	if err := http.ListenAndServe(*listen, s); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
