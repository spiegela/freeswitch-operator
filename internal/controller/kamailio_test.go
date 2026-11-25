/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestServiceRef(t *testing.T) {
	for _, tc := range []struct {
		url  string
		key  types.NamespacedName
		port int32
		ok   bool
	}{
		{"http://kamailio:5070/RPC", types.NamespacedName{Namespace: "telephony", Name: "kamailio"}, 5070, true},
		{"http://kamailio.sip:5070/RPC", types.NamespacedName{Namespace: "sip", Name: "kamailio"}, 5070, true},
		{"http://kamailio.sip.svc:5070/RPC", types.NamespacedName{Namespace: "sip", Name: "kamailio"}, 5070, true},
		{"http://kamailio.sip.svc.cluster.local/RPC", types.NamespacedName{Namespace: "sip", Name: "kamailio"}, 80, true},
		{"https://kamailio.sip.svc.cluster.local./RPC", types.NamespacedName{Namespace: "sip", Name: "kamailio"}, 443, true},
		{"http://10.0.0.5:5070/RPC", types.NamespacedName{}, 0, false},
		{"http://[fd00::5]:5070/RPC", types.NamespacedName{}, 0, false},
		{"http://rpc.kamailio.example.com:5070/RPC", types.NamespacedName{}, 0, false},
		{"not a url", types.NamespacedName{}, 0, false},
	} {
		key, port, ok := serviceRef(tc.url, "telephony")
		if ok != tc.ok || (ok && (key != tc.key || port != tc.port)) {
			t.Errorf("serviceRef(%q) = %v %d %v, want %v %d %v", tc.url, key, port, ok, tc.key, tc.port, tc.ok)
		}
	}
}

func TestEndpointURLsMapsTheServicePortAndSkipsUnready(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "sip", Port: 5060},
		{Name: "rpc", Port: 80},
	}}}
	slices := []discoveryv1.EndpointSlice{{
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("sip"), Port: ptr.To[int32](5060)}, {Name: ptr.To("rpc"), Port: ptr.To[int32](5070)}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
			{Addresses: []string{"10.0.0.1"}}, // Ready unset counts as ready
			{Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}},
			{Addresses: []string{"10.0.0.4"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Terminating: ptr.To(true)}},
			// Published while not ready (publishNotReadyAddresses): Ready is always true, Serving is the pod's.
			{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Serving: ptr.To(false)}},
			{Addresses: []string{"10.0.0.6"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Serving: ptr.To(true)}},
		},
	}, {
		// A slice without the port (mid-update) contributes nothing.
		Ports:     []discoveryv1.EndpointPort{{Name: ptr.To("sip"), Port: ptr.To[int32](5060)}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.9"}}},
	}, {
		// The same address in a second slice is called once.
		Ports:     []discoveryv1.EndpointPort{{Name: ptr.To("rpc"), Port: ptr.To[int32](5070)}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}}},
	}}
	got, err := endpointURLs("http://kamailio.sip.svc/RPC?x=1", svc, 80, slices)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://10.0.0.1:5070/RPC?x=1", "http://10.0.0.2:5070/RPC?x=1", "http://10.0.0.6:5070/RPC?x=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEndpointURLsKeepsTheURLPortForAHeadlessService(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
	slices := []discoveryv1.EndpointSlice{{
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"fd00::1"}}},
	}}
	got, err := endpointURLs("http://kamailio:5070/RPC", svc, 5070, slices)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http://[fd00::1]:5070/RPC"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

type recordingNotifier struct {
	url   string
	calls *[]string
	fail  bool
}

func (n recordingNotifier) SetInactive(_ context.Context, _ int32, uri string) error {
	*n.calls = append(*n.calls, n.url+" set_state "+uri)
	if n.fail {
		return errors.New("down")
	}
	return nil
}

func (n recordingNotifier) Admit(_ context.Context, addrs []string) error {
	*n.calls = append(*n.calls, n.url+" admit "+strings.Join(addrs, ","))
	if n.fail {
		return errors.New("down")
	}
	return nil
}

func (n recordingNotifier) Reload(context.Context, map[int32][]string) error {
	*n.calls = append(*n.calls, n.url+" reload")
	if n.fail {
		return errors.New("down")
	}
	return nil
}

func TestOnlyListNotLoaded(t *testing.T) {
	stale := fmt.Errorf("%w: x", errListNotLoaded)
	down := errors.New("connection refused")
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{stale, true},
		{errors.Join(stale, stale), true},
		{errors.Join(stale, down), false},
		{down, false},
	} {
		if got := onlyListNotLoaded(tc.err); got != tc.want {
			t.Errorf("onlyListNotLoaded(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestFanoutNotifierTellsEveryPodAndReportsEachFailure(t *testing.T) {
	var calls []string
	f := &fanoutNotifier{service: "Service sip/kamailio", targets: []string{"http://a/RPC", "http://b/RPC", "http://c/RPC"}}
	for i, u := range f.targets {
		f.notifiers = append(f.notifiers, recordingNotifier{url: u, calls: &calls, fail: i == 0})
	}
	err := f.Reload(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "http://a/RPC (Service sip/kamailio): down") {
		t.Fatalf("want the first pod's failure, got %v", err)
	}
	if err := f.SetInactive(context.Background(), 1, "sip:10.0.0.1:5060"); err == nil {
		t.Fatal("want an error")
	}
	want := []string{
		"http://a/RPC reload", "http://b/RPC reload", "http://c/RPC reload",
		"http://a/RPC set_state sip:10.0.0.1:5060", "http://b/RPC set_state sip:10.0.0.1:5060", "http://c/RPC set_state sip:10.0.0.1:5060",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("a failure must not stop the others:\n got %v\nwant %v", calls, want)
	}
	if err := (&fanoutNotifier{}).Reload(context.Background(), nil); err != nil {
		t.Fatalf("no pods, nothing to tell: %v", err)
	}
}
