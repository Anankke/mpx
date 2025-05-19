package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Anankke/mpx"
	"github.com/Anankke/mpx/dialer"
	"github.com/Anankke/mpx/mpx-tunnel/relay"
)

var (
	ListenAddr  = flag.String("listen", "0.0.0.0:5513", "")
	remoteAddr  = flag.String("server", "", "")
	targetAddr  = flag.String("target", "", "target address")
	coNum       = flag.Int("p", 2, "")
	enablePprof = flag.Bool("pprof", false, "")
	verbose     = flag.Bool("v", false, "verbose")
	maxFails    = flag.Int("max-fails", 2, "max consecutive dial failures before marking server failed")
	failTimeout = flag.Duration("fail-timeout", 30*time.Second, "duration to skip failed server before retrying")
	offset      = flag.Int("offset", 1, "port offset for port cycling on failure")
)

// isBenignRelayError returns true for expected errors when connections are closed.
func isBenignRelayError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") || strings.Contains(msg, "connection reset by peer")
}

// MultiServerPortCyclingDialer implements independent port cycling for each server.
// It only increments the port when a connection attempt to that server's port fails.

type serverPortCycler struct {
	baseAddr   string
	basePort   int
	offset     int
	cur        int
	weight     uint32
	lastTried  time.Time
}

type MultiServerPortCyclingDialer struct {
	servers     []*serverPortCycler
	failTimeout time.Duration
	mu          sync.Mutex
}

func (d *MultiServerPortCyclingDialer) Dial() (net.Conn, uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		soonest := time.Now().Add(d.failTimeout)
		found := false
		for _, s := range d.servers {
			wait := s.lastTried.Add(d.failTimeout).Sub(time.Now())
			if wait > 0 {
				if s.lastTried.Add(d.failTimeout).Before(soonest) {
					soonest = s.lastTried.Add(d.failTimeout)
				}
				continue
			}
			port := s.basePort + s.cur
			addr := net.JoinHostPort(s.baseAddr, strconv.Itoa(port))
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			s.lastTried = time.Now()
			if err == nil {
				return conn, s.weight, nil
			}
			s.cur = (s.cur + 1) % s.offset
			found = true
		}
		if !found {
			// If all servers are cooling down, sleep until the next retry time.
			d.mu.Unlock()
			time.Sleep(time.Until(soonest))
			d.mu.Lock()
		}
	}
}

func main() {
	flag.Parse()
	if *enablePprof {
		go http.ListenAndServe("0.0.0.0:6060", nil)
	}
	if *verbose {
		mpx.Verbose(true)
	}

	// If remoteAddr is set, run in client mode; if targetAddr is set, run in server mode. At least one must be specified.
	if *remoteAddr != "" {
		runClient(*ListenAddr, *remoteAddr, *coNum)
	} else if *targetAddr != "" {
		runServer(*ListenAddr, *targetAddr)
	} else {
		log.Fatal("server or target address must be set")
	}
}

func runClient(localAddr, remoteAddr string, concurrentNum int) {
	servers := strings.Split(remoteAddr, ",")
	remoteAddrs := make([]dialer.ServerWithWeight, 0, len(servers))
	for _, server := range servers {
		re := strings.Split(server, "|")
		if len(re) == 2 {
			addr := re[0]
			weight, err := strconv.Atoi(re[1])
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("add server %s with weight %d", addr, weight)
			remoteAddrs = append(remoteAddrs, dialer.ServerWithWeight{
				Addr:   addr,
				Weight: uint32(weight),
			})
		}
	}

	// Normalize and validate the address format for each remote server, ensuring host:port is correct.
	cyclers := make([]*serverPortCycler, 0, len(remoteAddrs))
	for _, s := range remoteAddrs {
		host, portStr, err := net.SplitHostPort(s.Addr)
		if err != nil {
			log.Fatalf("invalid server address: %s", s.Addr)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			log.Fatalf("invalid port: %s", portStr)
		}
		if port+*offset-1 > 65535 {
			log.Fatalf("port offset out of range: base %d + offset %d > 65535", port, *offset)
		}
		cyclers = append(cyclers, &serverPortCycler{
			baseAddr:  host,
			basePort:  port,
			offset:    *offset,
			cur:       0,
			weight:    s.Weight,
			lastTried: time.Time{},
		})
	}

	d := &MultiServerPortCyclingDialer{servers: cyclers, failTimeout: *failTimeout}
	cp := mpx.NewConnPool()
	cp.StartWithDialer(d, concurrentNum)
	lis, err := net.Listen("tcp", localAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Start at %s", lis.Addr().String())
	for {
		c, err := lis.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go func() {
			defer c.Close()
			rc, err := cp.Connect(nil)
			if err != nil {
				log.Printf("failed to connect to target: %v", err)
				return
			}
			_, _, err = relay.Relay(c, rc)
			if err != nil && !isBenignRelayError(err) {
				log.Printf("relay error: %v", err)
			}
		}()
	}
}

func runServer(localAddr, targetAddr string) {
	target, err := net.ResolveTCPAddr("tcp", targetAddr)
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", localAddr)
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
			if *verbose {
				log.Printf("dial to %s", targetAddr)
			}
			rc, err := net.DialTCP("tcp", nil, target)
			if err != nil || rc == nil {
				log.Printf("failed to connect to target[%s]: %v", targetAddr, err)
				return
			}
			rc.SetKeepAlive(true)
			_, _, err = relay.Relay(tunn, rc)
			if err != nil {
				if !isBenignRelayError(err) {
					log.Print(err)
				}
				return
			}
		}()
	}
}
