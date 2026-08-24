// Command response-loss-proxy runs the one-shot post-commit response-loss fixture.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/edu-agent/edu-agent/contracttests/cli-m1/responseproxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18082", "listen address")
	upstream := flag.String("upstream", "", "absolute upstream HTTP(S) URL")
	controlKey := flag.String("control-key", "response-loss-control-key", "fixture control key")
	flag.Parse()
	if *upstream == "" {
		log.Fatal("-upstream is required")
	}
	proxy, err := responseproxy.New(*upstream, responseproxy.Options{ControlKey: *controlKey})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("response-loss proxy listening on %s for upstream %s", *listen, *upstream)
	log.Fatal(http.ListenAndServe(*listen, proxy))
}
