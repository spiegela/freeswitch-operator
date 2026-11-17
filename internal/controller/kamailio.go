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
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kamailio usually runs as several pods behind one Service, and a call to the Service reaches only one of them.
// So when spec.dispatcher.jsonrpcURL names a Service in the cluster, the operator sends each JSON-RPC call to
// every ready pod behind it instead, and every Kamailio hears about each change. Any other URL is called as given.

// serviceRef reads a Service reference out of a JSON-RPC URL whose host is a cluster Service name: "name" (in
// defaultNamespace), "name.namespace", or "name.namespace.svc" with or without the cluster domain. ok is false for
// an IP address or any other host. port is the URL's port, or the scheme's default.
func serviceRef(rawURL, defaultNamespace string) (key types.NamespacedName, port int32, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return key, 0, false
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return key, 0, false
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	switch {
	case len(labels) == 1:
		key = types.NamespacedName{Namespace: defaultNamespace, Name: labels[0]}
	case len(labels) == 2, len(labels) >= 3 && labels[2] == "svc":
		key = types.NamespacedName{Namespace: labels[1], Name: labels[0]}
	default:
		return key, 0, false
	}
	switch p := u.Port(); {
	case p != "":
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return key, 0, false
		}
		port = int32(n)
	case u.Scheme == "https":
		port = 443
	default:
		port = 80
	}
	return key, port, true
}

// endpointURLs rewrites rawURL once for each ready, non-terminating endpoint of the Service, with the endpoint's
// address and the target port behind the Service port the URL names. A Service with no such port (a headless
// one, say) keeps the URL's port. The result is sorted, without duplicates.
func endpointURLs(rawURL string, svc *corev1.Service, servicePort int32, endpointSlices []discoveryv1.EndpointSlice) ([]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	portName, matched := "", false
	for _, p := range svc.Spec.Ports {
		if p.Port == servicePort {
			portName, matched = p.Name, true
			break
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range endpointSlices {
		port := servicePort
		if matched {
			found := false
			for _, p := range s.Ports {
				if p.Port != nil && (p.Name == nil && portName == "" || p.Name != nil && *p.Name == portName) {
					port, found = *p.Port, true
					break
				}
			}
			if !found {
				continue
			}
		}
		for _, ep := range s.Endpoints {
			// An unset Ready means ready (EndpointConditions); a terminating Kamailio needs no news.
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			for _, addr := range ep.Addresses {
				t := *u
				t.Host = net.JoinHostPort(addr, strconv.Itoa(int(port)))
				if s := t.String(); !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// dispatcherNotifier builds the notifier for spec.dispatcher.jsonrpcURL: one call per ready Kamailio pod when the
// URL names a Service in the cluster, the URL itself otherwise. It returns nil when notification is off.
func (r *FreeswitchClusterReconciler) dispatcherNotifier(ctx context.Context, namespace, rawURL string) (DispatcherNotifier, error) {
	if rawURL == "" || r.NewNotifier == nil {
		return nil, nil
	}
	key, port, ok := serviceRef(rawURL, namespace)
	if !ok {
		return r.NewNotifier(rawURL), nil
	}
	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			// Not a Service of this cluster (an outside name such as kamailio.example): call it as given.
			return r.NewNotifier(rawURL), nil
		}
		return nil, err
	}
	var endpointSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &endpointSlices, client.InNamespace(key.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: key.Name}); err != nil {
		return nil, err
	}
	targets, err := endpointURLs(rawURL, &svc, port, endpointSlices.Items)
	if err != nil {
		return nil, err
	}
	f := &fanoutNotifier{service: key.String()}
	for _, t := range targets {
		f.targets = append(f.targets, t)
		f.notifiers = append(f.notifiers, r.NewNotifier(t))
	}
	return f, nil
}

// fanoutNotifier makes each call on every Kamailio pod behind a Service. It tries them all even when one fails,
// and reports every failure. With no ready pod there is no one to tell: a Kamailio that starts later reads the
// current list from disk.
type fanoutNotifier struct {
	service   string
	targets   []string
	notifiers []DispatcherNotifier
}

func (f *fanoutNotifier) SetInactive(ctx context.Context, setID int32, uri string) error {
	return f.each(func(n DispatcherNotifier) error { return n.SetInactive(ctx, setID, uri) })
}

func (f *fanoutNotifier) Reload(ctx context.Context, setID int32, want []string) error {
	return f.each(func(n DispatcherNotifier) error { return n.Reload(ctx, setID, want) })
}

func (f *fanoutNotifier) each(call func(DispatcherNotifier) error) error {
	var errs []error
	for i, n := range f.notifiers {
		if err := call(n); err != nil {
			errs = append(errs, fmt.Errorf("%s (Service %s): %w", f.targets[i], f.service, err))
		}
	}
	return errors.Join(errs...)
}
