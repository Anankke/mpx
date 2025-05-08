package main

import (
	"flag"
	"log"
	"net"

	"github.com/Anankke/mpx"
	"github.com/Anankke/mpx/mpx-tunnel/relay"
)

var (
	ListenAddr = flag.String("l", "0.0.0.0:5512", "listen address")
	targetAddr = flag.String("target", "", "target address")
	verbose    = flag.Bool("v", false, "verbose")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	mpx.Verbose(*verbose)
	target, err := net.ResolveTCPAddr("tcp", *targetAddr)
	if err != err {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", *ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	cp := mpx.NewConnPool()
	go cp.ServeWithListener(lis)
	log.Printf("Start at %s", lis.Addr().String())

	for {
		tunn, err := cp.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		if tunn == nil {
			continue
		}
		go func() {
			defer tunn.Close()
			log.Printf("dial to %s", *targetAddr)
			rc, err := net.DialTCP("tcp", nil, target)
			if err != nil || rc == nil {
				log.Printf("failed to connect to target[%s]: %v", *targetAddr, err)
				return
			}
			rc.SetKeepAlive(true)
			_, _, err = relay.Relay(tunn, rc)
			if err != nil {
				log.Print(err)
				return
			}
		}()
	}
}
