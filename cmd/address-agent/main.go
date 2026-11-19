//go:build linux

// The address agent: one per node (a DaemonSet with host networking and NET_ADMIN), it puts the bind addresses
// SIPAddressPools give its node on the node's interface and reports them on its own pod. See internal/addressagent.
package main

import (
	"context"
	"flag"
	"net/netip"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
	"git.instruments.com/ci/freeswitch-operator/internal/addressagent"
)

// resync repairs an address something else took off the interface.
const resync = 30 * time.Second

func main() {
	var probeAddr string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("address-agent")

	node, ns, name := os.Getenv("NODE_NAME"), os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME")
	hostIP, err := netip.ParseAddr(os.Getenv("HOST_IP"))
	if node == "" || ns == "" || name == "" || err != nil {
		log.Error(err, "NODE_NAME, POD_NAMESPACE, POD_NAME and HOST_IP must be set (downward API)")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			// Only its own pod: the agent needs no other.
			&corev1.Pod{}: {Namespaces: map[string]cache.Config{ns: {}}, Field: fields.OneTermEqualSelector("metadata.name", name)},
		}},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}
	agent := &addressagent.Agent{
		Client: mgr.GetClient(), Links: addressagent.Netlink{},
		Node: node, HostIP: hostIP, Pod: types.NamespacedName{Namespace: ns, Name: name},
	}
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: node}}}
	})
	err = ctrl.NewControllerManagedBy(mgr).
		Named("addressagent").
		Watches(&sipv1alpha1.SIPAddressPool{}, one).
		Complete(reconcile.Func(func(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
			return reconcile.Result{RequeueAfter: resync}, agent.Sync(ctx)
		}))
	if err != nil {
		log.Error(err, "unable to create controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	log.Info("starting", "node", node, "hostIP", hostIP)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}
