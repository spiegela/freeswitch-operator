package controller

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// sipProbeTick is how often the prober looks for members due a probe.
const sipProbeTick = time.Second

// sipProbeConcurrency bounds the probes in flight at once.
const sipProbeConcurrency = 64

// ProbeFunc sends one SIP OPTIONS to host:port over transport and reports whether any SIP response came back within
// timeout.
type ProbeFunc func(ctx context.Context, host string, port int32, transport sipv1alpha1.DispatcherTransport, timeout time.Duration) error

// SIPProber sends SIP OPTIONS to every scheduled FreeSWITCH pod of every fleet, each at its fleet's
// spec.failover.sipProbe period, and keeps each pod's run of failures in memory. When a pod's signal starts or stops
// failing it sends the pod's fleet on Events, so the fleet's health is decided again at once. It runs on the leader
// only. After a restart it knows nothing until each pod's probes fail failureThreshold times again, which only
// delays a verdict.
type SIPProber struct {
	Client client.Reader
	// Events receives a fleet (a FreeswitchCluster with only its name and namespace) whose pod's signal changed. A
	// send waits while the channel is full, holding up only the probe that made it, so no change is lost.
	Events chan event.GenericEvent
	// Probe is the probe; nil means SIP OPTIONS (SIPOptions).
	Probe ProbeFunc
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	state map[types.UID]*probeState
}

type probeState struct {
	next        time.Time
	inFlight    bool
	failures    int
	firstFailed time.Time
	lastErr     string
	failing     bool
	seen        bool
}

func (p *SIPProber) now() time.Time { return nowFrom(p.Now) }

// Result implements SIPProbeResults.
func (p *SIPProber) Result(uid types.UID) (sipProbeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.state[uid]
	if !ok || !s.seen {
		return sipProbeResult{}, false
	}
	return sipProbeResult{Failing: s.failing, Since: s.firstFailed, Message: s.lastErr}, true
}

// Start implements manager.Runnable.
func (p *SIPProber) Start(ctx context.Context) error {
	t := time.NewTicker(sipProbeTick)
	defer t.Stop()
	sem := make(chan struct{}, sipProbeConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.tick(ctx, sem, &wg)
		}
	}
}

// probeTarget is one pod due a probe.
type probeTarget struct {
	uid       types.UID
	fleet     fleetRef
	host      string
	port      int32
	transport sipv1alpha1.DispatcherTransport
	cfg       failoverSettings
}

// tick starts a probe of every pod that is due one, and forgets pods that have gone.
func (p *SIPProber) tick(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	log := logf.FromContext(ctx).WithName("sip-prober")
	// The prober only reads what it lists, every second, so it skips the cache's deep copies.
	var fleets sipv1alpha1.FreeswitchClusterList
	if err := p.Client.List(ctx, &fleets, client.UnsafeDisableDeepCopy); err != nil {
		log.Error(err, "listing fleets to probe")
		return
	}
	var targets []probeTarget
	live := map[types.UID]bool{}
	for i := range fleets.Items {
		fsc := &fleets.Items[i]
		if fsc.DeletionTimestamp != nil {
			continue
		}
		cfg := resolve(fsc)
		var pods corev1.PodList
		if err := p.Client.List(ctx, &pods, client.InNamespace(fsc.Namespace), client.MatchingLabels(selectorLabels(fsc)), client.UnsafeDisableDeepCopy); err != nil {
			log.Error(err, "listing pods to probe", "fleet", fsc.Name, "namespace", fsc.Namespace)
			continue
		}
		for j := range pods.Items {
			pod := &pods.Items[j]
			if pod.Spec.NodeName == "" || pod.Status.PodIP == "" {
				continue
			}
			live[pod.UID] = true
			targets = append(targets, probeTarget{
				uid: pod.UID, fleet: fleetRef{kind: kindFreeswitch, namespace: fsc.Namespace, name: fsc.Name},
				host: pod.Status.PodIP, port: cfg.sipPort, transport: cfg.dispatcherProto, cfg: cfg.failover,
			})
		}
	}

	now := p.now()
	p.mu.Lock()
	if p.state == nil {
		p.state = map[types.UID]*probeState{}
	}
	for uid, s := range p.state {
		if !live[uid] && !s.inFlight {
			delete(p.state, uid)
		}
	}
	var due []probeTarget
	for _, t := range targets {
		s := p.state[t.uid]
		if s == nil {
			s = &probeState{}
			p.state[t.uid] = s
		}
		if s.inFlight || now.Before(s.next) {
			continue
		}
		s.inFlight, s.next = true, now.Add(t.cfg.probePeriod)
		due = append(due, t)
	}
	p.mu.Unlock()

	probe := p.Probe
	if probe == nil {
		probe = SIPOptions
	}
	for _, t := range due {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			p.release(due)
			return
		}
		wg.Go(func() {
			defer func() { <-sem }()
			start := p.now()
			err := probe(ctx, t.host, t.port, t.transport, t.cfg.probeTimeout)
			if ctx.Err() != nil {
				p.release([]probeTarget{t})
				return
			}
			recordSIPProbe(t.fleet, err)
			p.record(ctx, t, start, err)
		})
	}
}

// release clears inFlight for probes that were never run.
func (p *SIPProber) release(ts []probeTarget) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range ts {
		if s := p.state[t.uid]; s != nil {
			s.inFlight = false
		}
	}
}

// record notes one probe's outcome and sends the fleet on Events when the pod's signal changes.
func (p *SIPProber) record(ctx context.Context, t probeTarget, at time.Time, err error) {
	p.mu.Lock()
	s := p.state[t.uid]
	if s == nil {
		p.mu.Unlock()
		return
	}
	s.inFlight, s.seen = false, true
	was := s.failing
	if err == nil {
		s.failures, s.firstFailed, s.lastErr, s.failing = 0, time.Time{}, "", false
	} else {
		if s.failures == 0 {
			s.firstFailed = at
		}
		s.failures++
		s.lastErr = "SIP OPTIONS probes unanswered: " + probeErrorText(err)
		s.failing = s.failures >= t.cfg.probeThreshold
	}
	changed := was != s.failing
	p.mu.Unlock()
	if changed && p.Events != nil {
		ev := event.GenericEvent{Object: &sipv1alpha1.FreeswitchCluster{ObjectMeta: metav1.ObjectMeta{Namespace: t.fleet.namespace, Name: t.fleet.name}}}
		select {
		case p.Events <- ev:
		case <-ctx.Done():
		}
	}
}

// probeErrorText is a probe's error without the addresses a dial or read adds (its local port changes with every
// probe), so that the member's status, which shows it, stays the same for as long as the failure does.
func probeErrorText(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

// SIPOptions sends a SIP OPTIONS request to host:port and waits up to timeout for any SIP response to it (any
// status: a 403 from an ACL, or a 503 while busy, still shows a live SIP stack). Over UDP it retransmits as RFC 3261
// timer E does (500 ms, doubling), and a port that answers with ICMP unreachable fails at once.
func SIPOptions(ctx context.Context, host string, port int32, transport sipv1alpha1.DispatcherTransport, timeout time.Duration) error {
	transport = cmp.Or(transport, sipv1alpha1.DispatcherTransportUDP)
	network := string(transport)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	callID := randomToken(12) + "@freeswitch-operator"
	req := optionsRequest(conn.LocalAddr(), host, port, transport, callID)

	if transport == sipv1alpha1.DispatcherTransportTCP {
		_ = conn.SetDeadline(deadline)
		if _, err := conn.Write(req); err != nil {
			return err
		}
		return readSIPResponse(bufio.NewReader(conn), callID)
	}

	// Only a response's start line and headers are read; a bigger datagram loses only the rest of its body.
	buf := make([]byte, 8192)
	interval := 500 * time.Millisecond
	for {
		if _, err := conn.Write(req); err != nil {
			return err
		}
		next := time.Now().Add(interval)
		if next.After(deadline) {
			next = deadline
		}
		_ = conn.SetReadDeadline(next)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return err
			}
			if readSIPResponse(bufio.NewReader(bytes.NewReader(buf[:n])), callID) == nil {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("no answer from %s:%d/%s within %s", host, port, network, timeout)
		}
		interval = min(2*interval, 4*time.Second)
	}
}

func optionsRequest(local net.Addr, host string, port int32, transport sipv1alpha1.DispatcherTransport, callID string) []byte {
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	uri := "sip:" + target
	if transport == sipv1alpha1.DispatcherTransportTCP {
		uri += ";transport=tcp"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "OPTIONS %s SIP/2.0\r\n", uri)
	fmt.Fprintf(&b, "Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport\r\n", strings.ToUpper(string(transport)), local.String(), randomToken(10))
	b.WriteString("Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: <sip:health@freeswitch-operator>;tag=%s\r\n", randomToken(6))
	fmt.Fprintf(&b, "To: <%s>\r\n", uri)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", callID)
	b.WriteString("CSeq: 1 OPTIONS\r\n")
	b.WriteString("Accept: application/sdp\r\n")
	b.WriteString("User-Agent: freeswitch-operator health probe\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")
	return []byte(b.String())
}

// readSIPResponse reads one SIP message's start line and headers, and accepts any response to callID.
func readSIPResponse(r *bufio.Reader, callID string) error {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "SIP/2.0 ") {
		return fmt.Errorf("not a SIP response: %q", line)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		return err
	}
	got := hdr.Get("Call-Id")
	if got == "" {
		got = hdr.Get("I") // compact form
	}
	if got != callID {
		return fmt.Errorf("a response to another request (Call-ID %q)", got)
	}
	return nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
