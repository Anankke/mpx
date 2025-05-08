package dialer

import (
	"container/list"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type TCPDialer struct {
	RemoteAddr string
}

func (d *TCPDialer) Dial() (net.Conn, uint32, error) {
	conn, err := net.Dial("tcp", d.RemoteAddr)
	return conn, 1, err
}

type ServerWithWeight struct {
	Addr   string
	Weight uint32
}

type TCPmultiDialer struct {
	RemoteAddrs []ServerWithWeight
	dialLock    sync.Mutex
	connList    *list.List
	failCount   map[string]int
	failedAt    map[string]time.Time
	maxFails    int
	failTimeout time.Duration
}

type TCPmultiConn struct {
	net.Conn
	weight     uint32
	remoteAddr string
	isClosed   bool
}

func (c *TCPmultiConn) Close() error {
	c.isClosed = true
	return c.Conn.Close()
}

func NewTCPmultiDialer(remoteAddrs []ServerWithWeight, maxFails int, failTimeout time.Duration) *TCPmultiDialer {
	return &TCPmultiDialer{
		RemoteAddrs: remoteAddrs,
		connList:    list.New(),
		failCount:   make(map[string]int, len(remoteAddrs)),
		failedAt:    make(map[string]time.Time),
		maxFails:    maxFails,
		failTimeout: failTimeout,
	}
}

func (d *TCPmultiDialer) Dial() (net.Conn, uint32, error) {
	if len(d.RemoteAddrs) == 0 {
		return nil, 0, fmt.Errorf("no remote address")
	}
	d.dialLock.Lock()
	defer d.dialLock.Unlock()

	now := time.Now()
	eligible := make([]ServerWithWeight, 0, len(d.RemoteAddrs))
	for _, ra := range d.RemoteAddrs {
		if count := d.failCount[ra.Addr]; count >= d.maxFails {
			if t, ok := d.failedAt[ra.Addr]; ok {
				elapsed := now.Sub(t)
				if elapsed < d.failTimeout {
					continue // still in failure state
				}
				// reset after timeout
				log.Printf("Dialer: server %s recovered after %s, resetting failure state", ra.Addr, elapsed)
				d.failCount[ra.Addr] = 0
				delete(d.failedAt, ra.Addr)
			}
		}
		eligible = append(eligible, ra)
	}
	if len(eligible) == 0 {
		return nil, 0, fmt.Errorf("no available remote addresses: all in failure state")
	}

	weightMap := make(map[string]uint32, len(eligible))
	var totalWeight, totalActualWeight uint32
	for _, ra := range eligible {
		weightMap[ra.Addr] = 0
		totalWeight += ra.Weight
	}
	toDelete := make([]*list.Element, 0)
	for e := d.connList.Front(); e != nil; e = e.Next() {
		c := e.Value.(*TCPmultiConn)
		if c.isClosed {
			toDelete = append(toDelete, e)
			continue
		}
		if _, ok := weightMap[c.remoteAddr]; ok {
			weightMap[c.remoteAddr] += c.weight
			totalActualWeight += c.weight
		}
	}
	for _, e := range toDelete {
		d.connList.Remove(e)
	}

	var addr string
	var weight uint32
	if d.connList.Len() == 0 {
		addr = eligible[0].Addr
		weight = eligible[0].Weight
	} else {
		for _, ra := range eligible {
			if float32(weightMap[ra.Addr])/float32(totalActualWeight) <= float32(ra.Weight)/float32(totalWeight) {
				addr = ra.Addr
				weight = ra.Weight
				break
			}
		}
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, 0, err
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		d.failCount[addr]++
		if d.failCount[addr] >= d.maxFails {
			d.failedAt[addr] = now
		}
		return nil, 0, err
	}
	d.failCount[addr] = 0
	delete(d.failedAt, addr)
	conn.SetKeepAlive(true)
	conn.SetNoDelay(true)
	mc := &TCPmultiConn{
		Conn:       conn,
		weight:     weight,
		remoteAddr: addr,
	}
	d.connList.PushBack(mc)
	return mc, weight, nil
}
