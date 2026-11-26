package model

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// delayProxy forwards TCP connections to target and holds every chunk it
// reads for delay before it writes it on, both ways: each request and its
// answer then take one round trip of twice delay more, as an etcd behind a
// slow network does.
type delayProxy struct {
	ln     net.Listener
	target string
	delay  time.Duration
	wg     sync.WaitGroup
}

func newDelayProxy(t *testing.T, target string, delay time.Duration) *delayProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &delayProxy{ln: ln, target: target, delay: delay}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *delayProxy) serve() {
	defer p.wg.Done()
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		p.wg.Add(2)
		go p.pipe(in, out)
		go p.pipe(out, in)
	}
}

// pipe copies from src to dst, each chunk delivered delay after it was read,
// as a link of that latency carries it, whatever came before it: a reader
// stamps each chunk and a writer holds it until its time.
func (p *delayProxy) pipe(src, dst net.Conn) {
	defer p.wg.Done()
	type chunk struct {
		at   time.Time
		data []byte
	}
	queue := make(chan chunk, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range queue {
			time.Sleep(time.Until(c.at.Add(p.delay)))
			if _, err := dst.Write(c.data); err != nil {
				_ = src.Close()
				for range queue {
				}
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			queue <- chunk{at: time.Now(), data: append([]byte(nil), buf[:n]...)}
		}
		if err != nil {
			break
		}
	}
	close(queue)
	<-done
	_ = src.Close()
	_ = dst.Close()
}

// TestMeasurePoolLoad is the measurement of the multi wave plan's wave 2,
// change 13: the sp worker's load of a pool (LoadSpWithoutNodes, its pass's
// load, dnv-worker.md AR1) with 1024 volumes, through a proxy that adds a
// round trip of 0, 2, 5, 10 and 20 milliseconds to every etcd request, as a
// slow network to etcd does, and logs the load's time and that of one point
// read through the same proxy. The load's snapshot, its point reads
// included, runs under one budget, DefaultEtcdOpTimeout (EU5), and each
// range under one of its own. It runs only with DNV_MEASURE_LOAD=1.
func TestMeasurePoolLoad(t *testing.T) {
	if os.Getenv("DNV_MEASURE_LOAD") != "1" {
		t.Skip("a measurement: set DNV_MEASURE_LOAD=1 to run it")
	}
	endpoint, stop, err := startEtcd(findEtcdBin())
	if err != nil {
		t.Fatalf("start the test's own etcd: %v", err)
	}
	t.Cleanup(stop)
	direct := newClientAt(t, endpoint)
	ctx := context.Background()
	cid := testCid(t)
	conf := opsSpConf()
	conf.TdNameList = nil
	const volumes = 1024
	for idx := range volumes {
		name := fmt.Sprintf("vol-%04d", idx)
		conf.TdNameList = append(conf.TdNameList, name)
		mustPut(t, direct, ThinDeviceKey(cid, opsSpId, name), &pb.ThinDevice{
			TdId: 10000 + uint64(idx), DevId: uint32(idx + 1), Size: 1 << 30,
		})
	}
	mustPut(t, direct, SpConfKey(cid, opsSpName), conf)
	for _, rtt := range []time.Duration{0, 2, 5, 10, 20} {
		rtt *= time.Millisecond
		proxy := newDelayProxy(t, endpoint, rtt/2)
		cli, err := etcdutil.New(ctx, []string{proxy.ln.Addr().String()},
			5*time.Second)
		if err != nil {
			t.Fatalf("client through the proxy: %v", err)
		}
		// One point read's time, the mean of 20 after a first that opens
		// the connection.
		if _, err := cli.Get(ctx, SpConfKey(cid, opsSpName), &pb.SpConf{}); err != nil {
			t.Fatalf("a point read through the proxy: %v", err)
		}
		getStart := time.Now()
		for range 20 {
			if _, err := cli.Get(ctx, SpConfKey(cid, opsSpName),
				&pb.SpConf{}); err != nil {
				t.Fatalf("a point read through the proxy: %v", err)
			}
		}
		getTook := time.Since(getStart) / 20
		start := time.Now()
		state, err := LoadSpWithoutNodes(ctx, cli, cid, opsSpName)
		took := time.Since(start)
		_ = cli.Close()
		t.Logf("round trip %v: one point read took %v", rtt,
			getTook.Round(100*time.Microsecond))
		if err != nil {
			t.Logf("round trip %v: the load failed after %v: %v", rtt,
				took.Round(time.Millisecond), err)
			continue
		}
		t.Logf("round trip %v: the load of %d volumes took %v", rtt,
			len(state.Tds), took.Round(time.Millisecond))
	}
}
