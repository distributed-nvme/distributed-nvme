package main

import (
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const discNQN = "nqn.2014-08.org.nvmexpress.discovery"

type store struct {
	mu       sync.RWMutex
	model    *model
	filePath string
	genctr   uint64
	conns    map[string]*conn
}

func newStore(path string, m *model) *store {
	return &store{filePath: path, model: m, conns: make(map[string]*conn)}
}

func (s *store) curModel() *model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

func (s *store) curGenctr() uint64 {
	return atomic.LoadUint64(&s.genctr)
}

func (s *store) addConn(c *conn) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.conns[c.hostnqn]
	s.conns[c.hostnqn] = c
	return old
}

func (s *store) delConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.conns[c.hostnqn]; cur == c {
		delete(s.conns, c.hostnqn)
	}
}

func (s *store) watch(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastMtime int64
	if fi, err := os.Stat(s.filePath); err == nil {
		lastMtime = fi.ModTime().UnixNano()
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			fi, err := os.Stat(s.filePath)
			if err != nil {
				continue
			}
			mt := fi.ModTime().UnixNano()
			if mt == lastMtime {
				continue
			}
			lastMtime = mt
			s.onChange()
		}
	}
}

func (s *store) onChange() {
	newM, err := loadModel(s.filePath)
	if err != nil {
		log.Printf("[WARN] parse failed, keeping old model: %v", err)
		return
	}
	gen := atomic.AddUint64(&s.genctr, 1)
	s.mu.Lock()
	s.model = newM
	conns := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.maybeAEN(newM, gen)
	}
}

func main() {
	listen := os.Getenv("CDC_LISTEN")
	if listen == "" {
		listen = "192.168.122.78:8009"
	}
	file := os.Getenv("CDC_FILE")
	if file == "" {
		file = "/etc/dnv-cdc/subsystems.json"
	}
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)

	var m *model
	if mi, err := loadModel(file); err == nil {
		m = mi
		log.Printf("loaded %d subsystems from %s", len(m.Subsystems), file)
	} else {
		m = &model{}
		log.Printf("no initial model at %s: %v (will pick up on first edit)", file, err)
	}
	st := newStore(file, m)
	atomic.StoreUint64(&st.genctr, 1)
	stop := make(chan struct{})
	defer close(stop)
	go st.watch(stop)

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalf("listen %s: %v", listen, err)
	}
	log.Printf("CDC listening on %s, watching %s", listen, file)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go serve(st, c)
	}
}
