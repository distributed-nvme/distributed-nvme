package cdc

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The liveness and concurrency properties of §5 that the deterministic §8
// tests cannot express: "an AEN is never lost" (§0 #7), "a host that has
// stopped reading cannot stall the watcher" (DS6 -> NP11), and "every
// goroutine and every host state dies with its connection" (NP13, DS7).
//
// These are the one place in the package that uses REAL time on purpose. They
// are not measuring a timer — they are running the real goroutines against
// each other under load and asserting that nothing wedges, leaks or is
// dropped, which is exactly what a fake clock would hide. Every assertion is
// on an outcome (an AEN arrived, a count reached zero, a bounded elapsed
// time), never on a sleep having been long enough.

func stressEntry(i int, allowed []string) *entry {
	return newEntry(cdcEntry(
		fmt.Sprintf("nqn.2024-01.dnv:ss%d", i),
		allowed,
		tcpConf(fmt.Sprintf("10.0.0.%d", i%250+1), "4420"),
	))
}

func TestStressHostsAndWatcher(t *testing.T) {
	before := runtime.NumGoroutine()
	ts := startServer(t)

	const hostCount = 8
	const connsPerHost = 3
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Churn goroutine: hammers the registry the way the watcher does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			k := entryKey{cid: 1, shard: uint32(i % 16), spId: uint64(i % 7), ssId: uint64(i % 5)}
			if i%13 == 0 {
				deliver(ts.reg.apply(ctx, k, nil))
				continue
			}
			if i%37 == 0 {
				m := map[entryKey]*entry{}
				for j := 0; j < 20; j++ {
					m[entryKey{cid: 1, shard: uint32(j % 16), spId: uint64(j % 7), ssId: uint64(j % 5)}] =
						stressEntry(i+j, nil)
				}
				deliver(ts.reg.replace(ctx, m))
				continue
			}
			var allowed []string
			if i%3 == 0 {
				allowed = []string{fmt.Sprintf("nqn.2024-01.dnv:h%d", i%hostCount)}
			}
			deliver(ts.reg.apply(ctx, k, stressEntry(i, allowed)))
		}
	}()

	for hi := 0; hi < hostCount; hi++ {
		for ci := 0; ci < connsPerHost; ci++ {
			wg.Add(1)
			go func(hi, ci int) {
				defer wg.Done()
				h := ts.dial()
				defer h.close()
				h.handshake()
				c := h.connect(
					fmt.Sprintf("nqn.2024-01.dnv:h%d", hi),
					common.NvmeDiscoveryNqn, 0,
					common.CdcMaxAdminSqSize-1, 0,
				)
				if c.status != statusSuccess {
					t.Errorf("connect: %#x", c.status)
					return
				}
				h.enableAen()
				for n := 0; n < 40; n++ {
					h.armAer()
					h.getLogPage(lidDiscovery, 4096, 0)
					h.keepAlive()
				}
			}(hi, ci)
		}
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	ts.stop()

	// Let helper goroutines drain.
	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines: before %d after %d\n%s", before, after, buf[:n])
	}
}

// The watcher and the server together, driven off the fake store, then
// cancelled: nothing must be left running.
func TestStressWatcherPlusServerShutdown(t *testing.T) {
	before := runtime.NumGoroutine()
	ts := startServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := newWatcher(ts.srv.deps, ts.reg, []uint32{0, 1, 2, 3})
	done := make(chan struct{})
	go func() { defer close(done); w.run(ctx) }()

	fw := ts.store.nextWatch(t)
	var hosts []*fakeHost
	for i := 0; i < 4; i++ {
		h := ts.dial()
		h.connectOk(fmt.Sprintf("nqn.2024-01.dnv:g%d", i))
		h.enableAen()
		h.armAer()
		hosts = append(hosts, h)
	}
	for i := 0; i < 200; i++ {
		fw.put(testKey(uint32(i%16), uint64(i), 1),
			cdcEntry(fmt.Sprintf("nqn.2024-01.dnv:x%d", i), nil,
				tcpConf("10.0.0.5", "4420")))
	}
	// Kill the hosts mid-flight.
	for _, h := range hosts {
		h.close()
	}
	cancel()
	<-done
	ts.stop()
	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines: before %d after %d\n%s", before, after, buf[:n])
	}
}

// §0 #7 "never lost": with exactly one AER armed at all times, every impact
// must produce an AEN.
func TestAenIsNeverLostUnderLoad(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk("nqn.2024-01.dnv:aen")
	h.enableAen()
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		h.armAer()
		k := entryKey{cid: 1, shard: 0, spId: uint64(i), ssId: 1}
		deliver(ts.reg.apply(ctx, k, stressEntry(i, nil)))
		c, ok := h.pollPending(3 * time.Second)
		if !ok {
			t.Fatalf("iteration %d: no aen", i)
		}
		if c.dw0 != aenDiscLogChanged {
			t.Fatalf("iteration %d: dw0 %#x", i, c.dw0)
		}
	}
}

// The impact races the arming: the AER is armed from one goroutine while the
// impact lands from another. The bit must never be dropped.
func TestAenImpactRacesArming(t *testing.T) {
	ts := startServer(t)
	ctx := context.Background()
	for round := 0; round < 60; round++ {
		h := ts.dial()
		h.connectOk("nqn.2024-01.dnv:race")
		h.enableAen()
		ready := make(chan struct{})
		go func() {
			<-ready
			k := entryKey{cid: 1, shard: 0, spId: uint64(round), ssId: 1}
			deliver(ts.reg.apply(ctx, k, stressEntry(round, nil)))
		}()
		close(ready)
		h.armAer()
		if _, ok := h.pollPending(3 * time.Second); !ok {
			t.Fatalf("round %d: aen lost", round)
		}
		h.close()
	}
}

// A host that stops reading must not stall the watcher, and shutdown must
// still complete promptly.
func TestDeafHostDoesNotStallWatcherOrShutdown(t *testing.T) {
	ts := startServer(t)
	h := ts.dial()
	h.connectOk("nqn.2024-01.dnv:deaf")
	h.enableAen()
	for i := 0; i < 4; i++ {
		h.armAer()
	}
	// Pipeline many big Get Log Page reads and never read the answers, so
	// the server's writer wedges with wmu held.
	for i := 0; i < 64; i++ {
		cid := h.allocCid()
		s := newSqe(opcGetLogPage, cid)
		setSgl(s, 1<<20)
		s[40] = lidDiscovery
		numd := uint32(1<<20)/4 - 1
		binary.LittleEndian.PutUint16(s[42:44], uint16(numd))
		binary.LittleEndian.PutUint16(s[44:46], uint16(numd>>16))
		h.send(s, nil)
	}
	time.Sleep(500 * time.Millisecond)

	// The watcher path must still run at full speed.
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 500; i++ {
		k := entryKey{cid: 1, shard: 0, spId: uint64(i), ssId: 1}
		deliver(ts.reg.apply(ctx, k, stressEntry(i, nil)))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("watcher stalled by a deaf host: %v", d)
	} else {
		t.Logf("500 applies with a wedged writer took %v", d)
	}

	stopStart := time.Now()
	ts.stop()
	if d := time.Since(stopStart); d > 3*time.Second {
		t.Errorf("shutdown took %v with a wedged writer", d)
	} else {
		t.Logf("shutdown with a wedged writer took %v", d)
	}
}

// Connect/disconnect churn against a churning registry.
func TestConnectChurnLeaksNothing(t *testing.T) {
	ts := startServer(t)
	ctx := context.Background()
	stop := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			k := entryKey{cid: 1, shard: uint32(i % 4), spId: uint64(i % 3), ssId: 1}
			if i%5 == 0 {
				deliver(ts.reg.apply(ctx, k, nil))
			} else {
				deliver(ts.reg.apply(ctx, k, stressEntry(i, nil)))
			}
		}
	}()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for n := 0; n < 60; n++ {
				h := ts.dial()
				h.connectOk(fmt.Sprintf("nqn.2024-01.dnv:c%d", g%3))
				h.enableAen()
				h.armAer()
				h.getLogPage(lidDiscovery, 4096, 0)
				h.close()
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	close(stop)
	// Every host state must be gone once every connection is.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts.reg.hostCount() == 0 && ts.srv.connCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := ts.reg.hostCount(); n != 0 {
		t.Errorf("host states leaked: %d", n)
	}
	if n := ts.srv.connCount(); n != 0 {
		t.Errorf("conns leaked: %d", n)
	}
}
