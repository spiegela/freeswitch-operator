# External addresses and address pools

Peers allow-list our addresses, so each node needs a fixed public address, and it has to survive the node being
replaced.

FreeSWITCH puts its public address in SDP (`ext-rtp-ip`). Its SIP headers keep its own address (`ext-sip-ip`),
since peers signal only through Kamailio, which reaches it there and advertises the public address itself. The
operator writes the public address on each pod as the annotation `sip.instruments.com/external-address`. The pod
reads it from its downward-API volume, which, unlike environment variables, updates in place. sipbench's `vars.xml`
uses FreeSWITCH's `exec-set` pre-processor to read it at start.

A fleet's `externalAddress.source` says where the address comes from:

- **`NodeExternalIP`:** the node's `ExternalIP`.
- **`AddressPool`:** a `SIPAddressPool` holds a fixed list of addresses and binds one to each edge node, Kamailio's
  and FreeSWITCH's alike. When a node is replaced, its address moves to the replacement. One spare node and one
  spare address are the norm. Pools work the same whatever provides the addresses: AWS Elastic IPs and MetalLB (on
  kind and on-prem) today; other clouds and plain routed addresses are on the [roadmap](../README.md#roadmap).
- **`None`:** no external address.

**Pools and providers.** A pool is cluster-scoped, like a `GatewayClass`: addresses and nodes are cluster-wide, and
two pools must never hold the same address. Its spec names exactly one provider, each with its own block, so a new
provider is a new block rather than a new resource:

```yaml
apiVersion: sip.instruments.com/v1alpha1
kind: SIPAddressPool
metadata:
  name: edge-media
spec:
  awsElasticIP:                     # Existing Elastic IPs, associated with each node's instance
    region: us-east-1
    allocationIDs: [eipalloc-…, eipalloc-…, eipalloc-…]
  # metalLB:                        # Addresses MetalLB announces (L2 or BGP)
  #   ipAddressPool: sip-edge       #   the MetalLB IPAddressPool that holds them
  #   addresses: [192.168.1.240, …]
  # static:                         # planned. Routed to the nodes by something else (BGP, keepalived, by hand)
  #   addresses: [198.51.100.10, …]
status:
  datapath: NAT                     # NAT | Local, from the provider
  fencing: Authoritative            # Authoritative | Advisory, from the provider
  addresses:
    - id: eipalloc-…
      address: 203.0.113.10
      state: Bound                  # Free | Bound | Idle | Conflict | Error
      node: ip-10-0-1-23
      nodeID: i-0abc…
      claimedBy: [{kind: FreeswitchCluster, namespace: telephony, name: edge, pod: edge-1}]
      lastClaimedBy: [{kind: FreeswitchCluster, namespace: telephony, name: edge, pod: edge-1}]
      # bindAddress: 10.0.1.57      # when pods bind something other than the node's own address
      # bindAddressReady: true      #   and the node's address agent has it on its interface
  conditions: []                    # Ready, Degraded, ProviderAvailable
```

A fleet sets `externalAddress: {source: AddressPool, addressPoolRef: {name: …}}`. Signalling and media can draw from
separate pools, even with different providers. The pool controller does everything that doesn't depend on the
provider, once, tested against a fake provider: which address goes to which node, keeping a node's address while it
lives, giving a replaced node's address to its replacement, holding the spare, conditions and status, and the pod
annotations. A provider ([`internal/addresspool`](../internal/addresspool/provider.go)) only names nodes, observes,
binds and unbinds:

```go
type Provider interface {
	Capabilities() Capabilities                     // Datapath (NAT | Local), Fencing (Authoritative | Advisory)
	NodeID(n *corev1.Node) (string, error)          // the provider's name for a node (an instance ID)
	Observe(ctx context.Context) ([]Address, error) // where each address is bound now; truth, not our status
	Bind(ctx context.Context, a Address, nodeID string) error // refuses an address bound anywhere
	Unbind(ctx context.Context, a Address) error    // Authoritative: works with the node dead or unreachable
}
```

The planner's rules, the same for every provider:
- A node running a pod that uses the pool holds one address, `Bound`, for as long as the node lives, through rolling
  updates and drains (a draining pod still claims it). A node with none of those pods keeps any address it has,
  `Idle`: a spare, or a node between pods.
- A node that needs an address gets, in order: a free or idle address one of its pods used last (`lastClaimedBy`),
  so a StatefulSet pod replaced onto a new node keeps its address; any free address; then an idle one, moved off its
  node. With none left, the pool is `Degraded` and the node's pods wait without an address (unready, when
  `required`).
- An address the pool bound to a node that has left the cluster is unbound and reused. One bound outside the cluster
  that the pool didn't put there is a `Conflict`, left alone.
- An address the provider can't report on is an `Error`, and its node keeps it, so a provider outage doesn't make
  pods unready or move addresses.
- A pod waits for its address before it starts. FreeSWITCH and Kamailio both read it once, at start, and a pool binds
  it only once the pod is on its node, which takes seconds on AWS; started sooner, they would advertise the node's
  private address for their whole life. Kamailio's entrypoint already waits when the address is `required`; a
  FreeSWITCH fleet that draws from a pool and requires its address gets an init container (`wait-for-address`, in
  the FreeSWITCH image) that waits for the downward-API file, which the kubelet fills in within seconds of the
  annotation (checked on kind). Fleets on other sources get no such container, so their pod template is unchanged.
- The pool rereads its provider 5 s after binding or unbinding, and every 5 minutes otherwise, to see changes made
  outside the operator. Deleting a pool leaves its addresses where they are, since they may be carrying calls.

**Datapath: how the address reaches the node.** Providers don't differ only in API calls, and the difference
changes what the fleets render:

| | `NAT` | `Local` |
| --- | --- | --- |
| Providers | AWS Elastic IP; GCP, Azure and OCI reserved addresses | MetalLB (L2 or BGP), kube-vip, keepalived or BGP on-prem |
| The node sees | only its private address; the cloud translates 1:1 | the address itself, on an interface |
| FreeSWITCH | `rtp-ip` private, `ext-rtp-ip` the public address | `rtp-ip` and `ext-rtp-ip` both the address |
| Kamailio | `listen` on the private address, `advertise` the public one | `listen` on the address, plus the private listener FreeSWITCH uses |
| Replies and outbound traffic | leave from the address through the translation | leave from the address only because the process binds to it |

So when a pool's address reaches the node at an address the node doesn't already have (an AWS pool with
`privateAddress: Secondary`, or a `Local` provider such as MetalLB), each pod gets a second annotation beside
`sip.instruments.com/external-address` (what it advertises): `sip.instruments.com/bind-address` (what it binds RTP,
or Kamailio's peer-facing listener, to), from the pool's `status.addresses[].bindAddress`. Pods of other pools bind
their node's own address, as they always have. FreeSWITCH's SIP profile stays on the private address either way:
Kamailio dispatches to it there, and peers only ever signal through Kamailio.

- **FreeSWITCH** gets a `bind-address` downward-API file (`FS_BIND_ADDRESS_FILE`) in a fleet that draws from a pool;
  sipbench's config ([`bin/podinfo.sh`](../sipbench/freeswitch/conf/bin/podinfo.sh)) sets `rtp-ip` from it,
  falling back to `local_ip_v4`, and then advertises the external address for RTP only (`ext-rtp-ip`). `ext-sip-ip`
  is always `local_ip_v4`, with a pool or without: the external address may reach only the bind address, where SIP
  doesn't listen, or reach the node only through the cloud's translation, and a Contact naming it strands the
  dialog's ACK and BYE or sends them the long way round. FreeSWITCH uses the `ext-` addresses only for peers outside
  the profile's `local-network-acl`, and with the default (`localnet.auto`) Kamailio, which every call comes
  through, is inside it: on a `NAT` pool carriers were then offered the private bind address, and their media never
  arrived. So the `trunks` profile sets `local-network-acl` to an ACL that matches nothing, and every caller gets
  the external address; callers inside the cluster (sipbench) reach it through the cloud's translation, so the
  node's security group must let them in. The operator renders Kamailio's config but not FreeSWITCH's: a
  FreeSWITCH config of your own needs the same, or its RTP leaves from the node's address and the pool's address
  never carries it.
- **Kamailio** with a bind address listens on it, advertising the public address, and separately on the pod's own
  address for FreeSWITCH, instead of on `0.0.0.0`, so replies leave from the address the request came to. A new
  request leaves from the side it is going to (`force_send_socket`: toward a node from the pod's address, toward a
  peer from the bind address); requests in a dialog follow the double Record-Route that leaves.
- **The address agent** ([`config/components/address-agent`](../config/components/address-agent/kustomization.yaml)), a
  DaemonSet in the manager's image with host networking and `NET_ADMIN`, puts each bind address its node is given on
  the interface that holds the node's own address (a /32 labelled `<interface>:sip`) and reports what its node has on
  its own pod (`sip.instruments.com/bind-addresses`). Whether an address needs it is the provider's to say: it reports
  a bind address for the binding, and the agent and the pool need to know nothing else about the provider. The pool
  reads the agents' reports and marks each such address `bindAddressReady` once its node's agent has it; only then
  does the address count as the node's for its pods, so the operator sets a pod's external and bind addresses
  together and a pod never starts binding an address that isn't there. A new agent pod that hasn't reported yet (a
  rollout) changes nothing; one that reports the address gone takes it back. The agent removes only addresses it
  labelled that no pool gives its node, and never one that a pool which can't read its provider (`ProviderAvailable`
  False) names anywhere in its status, so a provider outage never takes an address off a node while other pools'
  moved addresses still leave. Deleting a pool does take its bind addresses off the nodes' interfaces: delete one
  only when no fleet uses it.
- **A just-bound address** waits for the pool's next read (5 s) to confirm it, and to learn its bind address, before
  the node's pods are given it: its status shows a message until then.

**Fencing: what unbinding guarantees.** `Authoritative` means one API call takes the address off a node whether or
not the node is alive (AWS's `DisassociateAddress`). `Advisory` means a node cut off from the cluster but not from the
network may go on holding the address (MetalLB stops announcing it from that node, but the node's own interface
still has it). Failover (on the [roadmap](../README.md#roadmap)) will move addresses on its own only for
`Authoritative` pools, unless a pool opts in.

**Rules for every provider.**
- The operator never allocates or releases an address: peers allow-list them, so the pool is a fixed list. It only
  binds and unbinds, and only addresses listed in the pool.
- A pool is checked against the provider, not against its own status: after a restart it reads bindings back
  before acting.
- Providers are turned on with a manager flag (`--address-providers=aws,metallb`; repeated flags add up, so the
  kustomize components combine), so the operator holds the permissions of the providers in use only. A pool whose
  provider isn't enabled reports `ProviderUnavailable`.

**AWS Elastic IPs.** The pool lists existing allocation IDs (see
[`config/samples`](../config/samples/sip_v1alpha1_sipaddresspool.yaml)). A node is named by the instance in its
provider ID. Binding associates the Elastic IP with the primary private address of the instance's primary network
interface, naming the interface, since AWS refuses an instance ID alone once an instance has a second interface (EKS
nodes with the VPC CNI usually do). It uses `AllowReassociation=false`, so it never takes an address from anything
else, and it refuses an instance that already holds an Elastic IP outside the pool. An address associated with an
interface that belongs to no instance (a NAT gateway, a load balancer) is a `Conflict`; an allocation ID that doesn't
exist is an `Error` on that address alone. The manager's service account needs `ec2:DescribeAddresses`,
`ec2:DescribeNetworkInterfaces`, `ec2:AssociateAddress` and `ec2:DisassociateAddress`, through IRSA or EKS Pod
Identity.

`privateAddress: Secondary` is for nodes whose own address already holds an Elastic IP the pool must not touch (a
cluster endpoint, as on a one-node cluster). Binding assigns the node's primary interface a new secondary private
address, records it on the Elastic IP as the tag `sip.instruments.com/private-address`, and associates the Elastic
IP with it; a failed association takes the address back. Unbinding disassociates, then unassigns only the address
the tag names (never a primary address), and removes the tag. The address agent then puts the address on the node's
interface. This needs `ec2:AssignPrivateIpAddresses`, `ec2:UnassignPrivateIpAddresses`, and `ec2:CreateTags` and
`ec2:DeleteTags` on Elastic IPs for that one tag key. Don't use it where the CNI hands out the primary interface's
secondary addresses to pods (the VPC CNI does): it could hand out this one, fill the interface's address limit, or
release it as surplus, which disassociates the Elastic IP. So the pool refuses to bind a new address to a node
running the VPC CNI's `aws-node` pod (the node waits, and the pool's Ready condition says why); an address already
bound there stays. On EKS, use `Primary` on a dedicated edge node group, in public subnets: an Elastic IP's replies
must leave through an internet gateway, not a NAT gateway.

Enabling it: add [`config/components/aws-address-pool`](../config/components/aws-address-pool/kustomization.yaml)
(`--address-providers=aws`) to `config/default`'s components, and for `Secondary` pools
[`config/components/address-agent`](../config/components/address-agent/kustomization.yaml) as well (a Flux
Kustomization on `./config/default` takes `spec.components`). Give the manager's service account a role with
[`iam-policy.json`](../config/components/aws-address-pool/iam-policy.json) (its last two statements are for `Secondary`
only), by IRSA annotation or Pod Identity association.

**MetalLB.** MetalLB only announces addresses that belong to a `LoadBalancer` Service, and a Service can't carry
FreeSWITCH's 16k-port RTP range. So for each binding the pool keeps a Service of its own in the operator's namespace
(`sip-<pool>-<address>`) that asks MetalLB for the address (`metallb.io/loadBalancerIPs`, and
`metallb.io/address-pool` when the pool names `ipAddressPool`), with `externalTrafficPolicy: Local`, no selector,
and an EndpointSlice the pool writes naming the node. MetalLB announces the address from that node only and won't
give it to any other Service; the pool reports an address another Service asks for or holds as a `Conflict`, and
leaves it alone. One MetalLB hasn't assigned (not in an IPAddressPool) stays bound, with a message, but its node's
pods aren't given it and the node counts as waiting. A Service whose EndpointSlice has gone announces nothing, so
the pool counts its address as unbound and the next bind completes it. Taking an address out of the spec deletes its
Service, so MetalLB stops announcing it. The Service's one port is a placeholder
(discard, 9/UDP): kube-proxy rewrites traffic to a Service's ports, and SIP and RTP must reach the pods untouched.
The address agent (above) puts the address on the node's interface while it is bound there, so the node delivers
every port locally and the pods bind it; unbinding deletes the Service and the agent takes the address off. Fencing
is `Advisory`: a node cut off from the cluster keeps the address on its interface.

Enabling it: install MetalLB with an IPAddressPool holding the addresses and an L2Advertisement or BGPAdvertisement
for it, run MetalLB's speaker on the fleets' nodes (give it their tolerations), and add
[`config/components/metallb-address-pool`](../config/components/metallb-address-pool/kustomization.yaml) and
[`config/components/address-agent`](../config/components/address-agent/kustomization.yaml) to `config/default`'s
components. On kind ([development](development.md#the-edge-on-kind)), MetalLB in L2 mode on the Docker network gives the
FreeSWITCH fleet its addresses, so the pool, the agent and media to the pooled address are checked in CI.

