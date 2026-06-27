// cmd/cdp-relay/main.go
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	listen := flag.String("listen", ":9222", "address the relay listens on")
	upstream := flag.String("upstream", "127.0.0.1:9223", "Chromium CDP address (host:port)")
	flag.Parse()

	h := newRelayHandler(*upstream)
	log.Printf("cdp-relay: listening on %s, forwarding to %s", *listen, *upstream)
	if err := http.ListenAndServe(*listen, h); err != nil {
		log.Fatalf("cdp-relay: %v", err)
	}
}
