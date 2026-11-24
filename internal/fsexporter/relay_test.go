package fsexporter

import (
	"bufio"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// speechEvent is a detector event as FreeSWITCH sends it in event plain, trimmed: mod_speech_detect's, from silero.
func speechEvent(detector, event, uuid, agentLeg string) string {
	h := "Event-Subclass: speech_detect%3A%3Achange\nEvent-Name: CUSTOM\nEvent-Date-Timestamp: 1795229956696728\n" +
		"Unique-ID: " + uuid + "\n"
	if agentLeg != "" {
		h += "variable_barge_in_agent_uuid: " + agentLeg + "\n"
	}
	return h + "Speech-Event: " + event + "\nSpeech-Detector: " + detector + "\nSpeech-Engine: silero\n" +
		"Speech-At: 1795229956441\nSpeech-Probability: 0.986\n\n"
}

func TestDetectionOf(t *testing.T) {
	h := parseEvent([]byte(speechEvent("caller", "started", "in-1", "out-1")))
	d, target, ok := detectionOf(h)
	if !ok || target != "out-1" {
		t.Fatalf("caller detection: ok=%v target=%q, want the agent leg out-1", ok, target)
	}
	want := Detection{Event: "speech-started", Party: "caller", Engine: "silero", At: 1795229956441}
	if d.Probability == nil || *d.Probability != 0.986 {
		t.Errorf("probability %v, want 0.986", d.Probability)
	}
	d.Probability = nil
	if d != want {
		t.Errorf("got %+v, want %+v", d, want)
	}

	h = parseEvent([]byte(speechEvent("agent", "stopped", "out-1", "")))
	if d, target, ok = detectionOf(h); !ok || target != "out-1" || d.Party != "agent" || d.Event != "speech-stopped" {
		t.Errorf("agent detection: %+v target=%q ok=%v, want speech-stopped on its own leg", d, target, ok)
	}

	// webrtc: no probability. Without Speech-At, the event's own time.
	h = parseEvent([]byte("Event-Subclass: speech_detect%3A%3Achange\nEvent-Date-Timestamp: 1795229956696728\n" +
		"Unique-ID: in-2\nvariable_barge_in_agent_uuid: out-2\nSpeech-Event: started\nSpeech-Detector: caller\n" +
		"Speech-Engine: webrtc\n\n"))
	if d, target, ok = detectionOf(h); !ok || target != "out-2" || d.Engine != "webrtc" || d.At != 1795229956696 || d.Probability != nil {
		t.Errorf("webrtc detection: %+v target=%q ok=%v", d, target, ok)
	}

	// A caller leg that names no agent leg: a detection with nowhere to go.
	h = parseEvent([]byte(speechEvent("caller", "started", "in-3", "")))
	if _, target, ok = detectionOf(h); !ok || target != "" {
		t.Errorf("no agent leg: target=%q ok=%v, want an empty target", target, ok)
	}
	// Someone else's detector, or another event, is not ours.
	for _, ev := range []string{speechEvent("mine", "started", "x", "y"), speechEvent("caller", "speaking", "x", "y"),
		"Event-Subclass: sofia%3A%3Aregister\nUnique-ID: x\n\n"} {
		h = parseEvent([]byte(ev))
		if _, _, ok := detectionOf(h); ok {
			t.Errorf("took %q for a detection", ev[:40])
		}
	}
}

// fakeFS is an event socket: the first connection gets events, the second the commands, which it records.
type fakeFS struct {
	ln       net.Listener
	events   chan string
	commands chan string
}

func newFakeFS(t *testing.T) *fakeFS {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFS{ln: ln, events: make(chan string, 10), commands: make(chan string, 10)}
	go func() {
		for i := 0; ; i++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, i%2 == 0)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeFS) serve(c net.Conn, events bool) {
	defer func() { _ = c.Close() }()
	r := textproto.NewReader(bufio.NewReader(c))
	// readCmd reads one command: its line, then the blank line ending it.
	readCmd := func() (string, error) {
		line, err := r.ReadLine()
		for err == nil {
			var blank string
			if blank, err = r.ReadLine(); blank == "" {
				break
			}
		}
		return line, err
	}
	_, _ = fmt.Fprint(c, "Content-Type: auth/request\n\n")
	if _, err := readCmd(); err != nil {
		return
	}
	_, _ = fmt.Fprint(c, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
	if events {
		if _, err := readCmd(); err != nil { // the subscription
			return
		}
		_, _ = fmt.Fprint(c, "Content-Type: command/reply\nReply-Text: +OK event listener enabled plain\n\n")
		for ev := range f.events {
			_, _ = fmt.Fprintf(c, "Content-Length: %d\nContent-Type: text/event-plain\n\n%s", len(ev), ev)
		}
		return
	}
	for {
		line, err := readCmd()
		if err != nil {
			return
		}
		f.commands <- line
		out := "+OK Success\n"
		if strings.Contains(line, "gone") {
			out = "-ERR No such channel!\n"
		}
		_, _ = fmt.Fprintf(c, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(out), out)
	}
}

func TestRelaySendsInfo(t *testing.T) {
	fs := newFakeFS(t)
	r := NewRelay(fs.ln.Addr().String(), "ClueCon", time.Second, nil)
	go r.Run(t.Context())

	fs.events <- speechEvent("caller", "started", "in-1", "out-1")
	select {
	case cmd := <-fs.commands:
		want := `api uuid_send_info out-1 application x-barge-in+json ` +
			`{"event":"speech-started","party":"caller","engine":"silero","probability":0.986,"at":1795229956441}`
		if cmd != want {
			t.Errorf("sent\n  %s\nwant\n  %s", cmd, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no INFO sent")
	}
	fs.events <- speechEvent("caller", "stopped", "in-1", "") // nowhere to go
	fs.events <- speechEvent("agent", "started", "gone", "")  // its leg has ended
	select {
	case <-fs.commands:
	case <-time.After(5 * time.Second):
		t.Fatal("no INFO sent for the agent's detection")
	}
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(r.detections.WithLabelValues("agent", "speech-started", resultFailed)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the failed send was not counted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, c := range []struct {
		party, event, result string
		want                 float64
	}{
		{"caller", "speech-started", resultRelayed, 1},
		{"caller", "speech-stopped", resultNoTarget, 1},
	} {
		if got := testutil.ToFloat64(r.detections.WithLabelValues(c.party, c.event, c.result)); got != c.want {
			t.Errorf("%s %s %s: %v, want %v", c.party, c.event, c.result, got, c.want)
		}
	}
	if got := testutil.ToFloat64(r.connected); got != 1 {
		t.Errorf("connected %v, want 1", got)
	}
}
