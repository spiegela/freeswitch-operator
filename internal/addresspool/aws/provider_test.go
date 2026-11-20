package aws

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
	"git.instruments.com/ci/freeswitch-operator/internal/addresspool"
)

// fakeEC2 holds Elastic IPs and instances' primary interfaces, and follows EC2's rules where the provider relies
// on them: describing a missing allocation ID fails the whole call, and AllowReassociation=false refuses an
// address that is associated.
type fakeEC2 struct {
	addrs   map[string]*ec2types.Address // by allocation ID
	enis    map[string]ec2types.NetworkInterface
	assocN  int
	calls   []string
	failErr error
	// failAssociate makes AssociateAddress fail.
	failAssociate error
	nextIP        int
}

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{addrs: map[string]*ec2types.Address{}, enis: map[string]ec2types.NetworkInterface{}}
}

func (f *fakeEC2) addEIP(id, ip string) {
	f.addrs[id] = &ec2types.Address{AllocationId: awssdk.String(id), PublicIp: awssdk.String(ip)}
}

func (f *fakeEC2) addInstance(instance, eni, privateIP string) {
	f.enis[instance] = ec2types.NetworkInterface{
		NetworkInterfaceId: awssdk.String(eni), PrivateIpAddress: awssdk.String(privateIP),
		PrivateIpAddresses: []ec2types.NetworkInterfacePrivateIpAddress{{PrivateIpAddress: awssdk.String(privateIP), Primary: awssdk.Bool(true)}},
	}
}

// secondaries are eni-a's secondary private addresses.
func (f *fakeEC2) secondaries() []string {
	const eniID = "eni-a"
	var out []string
	for _, eni := range f.enis {
		if awssdk.ToString(eni.NetworkInterfaceId) != eniID {
			continue
		}
		for _, pa := range eni.PrivateIpAddresses {
			if !awssdk.ToBool(pa.Primary) {
				out = append(out, awssdk.ToString(pa.PrivateIpAddress))
			}
		}
	}
	return out
}

// tag is eipalloc-1's private address tag.
func (f *fakeEC2) tag() string {
	for _, t := range f.addrs["eipalloc-1"].Tags {
		if awssdk.ToString(t.Key) == PrivateAddressTag {
			return awssdk.ToString(t.Value)
		}
	}
	return ""
}

func (f *fakeEC2) AssignPrivateIpAddresses(_ context.Context, in *ec2.AssignPrivateIpAddressesInput, _ ...func(*ec2.Options)) (*ec2.AssignPrivateIpAddressesOutput, error) {
	f.calls = append(f.calls, "AssignPrivateIpAddresses "+awssdk.ToString(in.NetworkInterfaceId))
	for instance, eni := range f.enis {
		if awssdk.ToString(eni.NetworkInterfaceId) == awssdk.ToString(in.NetworkInterfaceId) {
			f.nextIP++
			ip := "10.0.9." + strconv.Itoa(f.nextIP)
			eni.PrivateIpAddresses = append(eni.PrivateIpAddresses, ec2types.NetworkInterfacePrivateIpAddress{PrivateIpAddress: awssdk.String(ip), Primary: awssdk.Bool(false)})
			f.enis[instance] = eni
			return &ec2.AssignPrivateIpAddressesOutput{AssignedPrivateIpAddresses: []ec2types.AssignedPrivateIpAddress{{PrivateIpAddress: awssdk.String(ip)}}}, nil
		}
	}
	return nil, notFound("InvalidNetworkInterfaceID.NotFound")
}

func (f *fakeEC2) UnassignPrivateIpAddresses(_ context.Context, in *ec2.UnassignPrivateIpAddressesInput, _ ...func(*ec2.Options)) (*ec2.UnassignPrivateIpAddressesOutput, error) {
	f.calls = append(f.calls, "UnassignPrivateIpAddresses "+awssdk.ToString(in.NetworkInterfaceId)+" "+strings.Join(in.PrivateIpAddresses, ","))
	for instance, eni := range f.enis {
		if awssdk.ToString(eni.NetworkInterfaceId) == awssdk.ToString(in.NetworkInterfaceId) {
			eni.PrivateIpAddresses = slices.DeleteFunc(slices.Clone(eni.PrivateIpAddresses), func(pa ec2types.NetworkInterfacePrivateIpAddress) bool {
				return slices.Contains(in.PrivateIpAddresses, awssdk.ToString(pa.PrivateIpAddress))
			})
			f.enis[instance] = eni
			return &ec2.UnassignPrivateIpAddressesOutput{}, nil
		}
	}
	return nil, notFound("InvalidNetworkInterfaceID.NotFound")
}

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.calls = append(f.calls, "CreateTags "+in.Resources[0])
	a := f.addrs[in.Resources[0]]
	a.Tags = append(a.Tags, in.Tags...)
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	f.calls = append(f.calls, "DeleteTags "+in.Resources[0])
	a := f.addrs[in.Resources[0]]
	a.Tags = slices.DeleteFunc(a.Tags, func(t ec2types.Tag) bool { return awssdk.ToString(t.Key) == awssdk.ToString(in.Tags[0].Key) })
	return &ec2.DeleteTagsOutput{}, nil
}

func notFound(code string) error { return &smithy.GenericAPIError{Code: code, Message: "not found"} }

func (f *fakeEC2) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	f.calls = append(f.calls, "DescribeAddresses")
	if f.failErr != nil {
		return nil, f.failErr
	}
	out := &ec2.DescribeAddressesOutput{}
	if len(in.Filters) > 0 { // instance-id
		for _, a := range f.addrs {
			if awssdk.ToString(a.InstanceId) == in.Filters[0].Values[0] {
				out.Addresses = append(out.Addresses, *a)
			}
		}
		return out, nil
	}
	for _, id := range in.AllocationIds {
		a, ok := f.addrs[id]
		if !ok {
			return nil, notFound("InvalidAllocationID.NotFound")
		}
		out.Addresses = append(out.Addresses, *a)
	}
	return out, nil
}

func (f *fakeEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	f.calls = append(f.calls, "DescribeNetworkInterfaces")
	out := &ec2.DescribeNetworkInterfacesOutput{}
	if awssdk.ToString(in.Filters[0].Name) == "addresses.private-ip-address" {
		for _, eni := range f.enis {
			for _, pa := range eni.PrivateIpAddresses {
				if awssdk.ToString(pa.PrivateIpAddress) == in.Filters[0].Values[0] {
					out.NetworkInterfaces = append(out.NetworkInterfaces, eni)
				}
			}
		}
		return out, nil
	}
	if eni, ok := f.enis[in.Filters[0].Values[0]]; ok && in.Filters[1].Values[0] == "0" {
		out.NetworkInterfaces = []ec2types.NetworkInterface{eni}
	}
	return out, nil
}

func (f *fakeEC2) AssociateAddress(_ context.Context, in *ec2.AssociateAddressInput, _ ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error) {
	f.calls = append(f.calls, "AssociateAddress "+awssdk.ToString(in.AllocationId)+" "+awssdk.ToString(in.NetworkInterfaceId)+" "+awssdk.ToString(in.PrivateIpAddress))
	if f.failAssociate != nil {
		return nil, f.failAssociate
	}
	a := f.addrs[awssdk.ToString(in.AllocationId)]
	if a.AssociationId != nil && !awssdk.ToBool(in.AllowReassociation) {
		return nil, &smithy.GenericAPIError{Code: "Resource.AlreadyAssociated"}
	}
	for instance, eni := range f.enis {
		if awssdk.ToString(eni.NetworkInterfaceId) == awssdk.ToString(in.NetworkInterfaceId) {
			f.assocN++
			a.AssociationId = awssdk.String("eipassoc-" + string(rune('0'+f.assocN)))
			a.InstanceId = awssdk.String(instance)
			a.NetworkInterfaceId = eni.NetworkInterfaceId
			a.PrivateIpAddress = in.PrivateIpAddress
		}
	}
	return &ec2.AssociateAddressOutput{AssociationId: a.AssociationId}, nil
}

func (f *fakeEC2) DisassociateAddress(_ context.Context, in *ec2.DisassociateAddressInput, _ ...func(*ec2.Options)) (*ec2.DisassociateAddressOutput, error) {
	f.calls = append(f.calls, "DisassociateAddress "+awssdk.ToString(in.AssociationId))
	for _, a := range f.addrs {
		if awssdk.ToString(a.AssociationId) == awssdk.ToString(in.AssociationId) {
			a.AssociationId, a.InstanceId, a.NetworkInterfaceId, a.PrivateIpAddress = nil, nil, nil, nil
			return &ec2.DisassociateAddressOutput{}, nil
		}
	}
	return nil, notFound("InvalidAssociationID.NotFound")
}

func pool(ids ...string) *sipv1alpha1.AWSElasticIPPool {
	return &sipv1alpha1.AWSElasticIPPool{Region: "us-east-1", AllocationIDs: ids}
}

func TestNodeID(t *testing.T) {
	p := New(newFakeEC2(), pool("eipalloc-1"))
	for providerID, want := range map[string]string{
		"aws:///us-east-1a/i-0abc123": "i-0abc123",
		"aws://us-east-1a/i-0abc123":  "i-0abc123",
		"kind://docker/edge/worker":   "",
		"aws:///us-east-1a/":          "",
		"":                            "",
	} {
		got, err := p.NodeID(&corev1.Node{Spec: corev1.NodeSpec{ProviderID: providerID}})
		if got != want || (want == "") != (err != nil) {
			t.Errorf("NodeID(%q) = %q, %v; want %q", providerID, got, err, want)
		}
	}
}

func TestBindObserveUnbind(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addEIP("eipalloc-2", "203.0.113.2")
	f.addInstance("i-a", "eni-a", "10.0.1.10")
	p := New(f, pool("eipalloc-1", "eipalloc-2"))

	if caps := p.Capabilities(); caps.Datapath != sipv1alpha1.AddressDatapathNAT || caps.Fencing != sipv1alpha1.AddressFencingAuthoritative {
		t.Fatalf("capabilities = %+v", caps)
	}
	obs, err := p.Observe(ctx)
	if err != nil || len(obs) != 2 || obs[0].IP != "203.0.113.1" || obs[0].NodeID != "" {
		t.Fatalf("Observe = %+v, %v", obs, err)
	}

	if err := p.Bind(ctx, obs[0], "i-a"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "AssociateAddress eipalloc-1 eni-a 10.0.1.10") {
		t.Errorf("did not associate through the primary interface and address: %v", f.calls)
	}
	obs, _ = p.Observe(ctx)
	if obs[0].NodeID != "i-a" || obs[0].BindingID == "" {
		t.Fatalf("after bind: %+v", obs[0])
	}

	// Another bind of an associated address is refused, not a silent move.
	f.addInstance("i-b", "eni-b", "10.0.1.11")
	if err := p.Bind(ctx, obs[0], "i-b"); err == nil {
		t.Error("rebinding an associated address succeeded")
	}

	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Errorf("unbinding an address already disassociated: %v", err)
	}
	if err := p.Unbind(ctx, addresspool.Address{ID: "eipalloc-2"}); err != nil {
		t.Errorf("unbinding an unbound address: %v", err)
	}
	obs, _ = p.Observe(ctx)
	if obs[0].NodeID != "" {
		t.Errorf("after unbind: %+v", obs[0])
	}
}

func TestBindRefusesAnInstanceHoldingAnotherElasticIP(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addEIP("eipalloc-other", "198.51.100.9")
	f.addInstance("i-a", "eni-a", "10.0.1.10")
	other := New(f, pool("eipalloc-other"))
	o, _ := other.Observe(ctx)
	if err := other.Bind(ctx, o[0], "i-a"); err != nil {
		t.Fatal(err)
	}

	p := New(f, pool("eipalloc-1"))
	obs, _ := p.Observe(ctx)
	err := p.Bind(ctx, obs[0], "i-a")
	if err == nil || !strings.Contains(err.Error(), "not in the pool") {
		t.Fatalf("Bind = %v, want a refusal", err)
	}
}

func secondaryPool(ids ...string) *sipv1alpha1.AWSElasticIPPool {
	p := pool(ids...)
	p.PrivateAddress = sipv1alpha1.AWSPrivateAddressSecondary
	return p
}

// A node whose own address already holds an Elastic IP (a cluster endpoint) takes a pool address on a secondary
// private address, and gives it back on unbinding, leaving its own Elastic IP alone.
func TestSecondaryBindObserveUnbind(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-endpoint", "198.51.100.9")
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addInstance("i-a", "eni-a", "10.0.1.10")
	endpoint := New(f, pool("eipalloc-endpoint"))
	e, _ := endpoint.Observe(ctx)
	if err := endpoint.Bind(ctx, e[0], "i-a"); err != nil {
		t.Fatal(err)
	}

	p := New(f, secondaryPool("eipalloc-1"))
	obs, _ := p.Observe(ctx)
	if err := p.Bind(ctx, obs[0], "i-a"); err != nil {
		t.Fatal(err)
	}
	if got := f.secondaries(); len(got) != 1 || f.tag() != got[0] {
		t.Fatalf("secondaries %v, tag %q", got, f.tag())
	}
	ip := f.secondaries()[0]
	if !slices.Contains(f.calls, "AssociateAddress eipalloc-1 eni-a "+ip) {
		t.Errorf("did not associate with the secondary address: %v", f.calls)
	}
	obs, _ = p.Observe(ctx)
	if obs[0].NodeID != "i-a" || obs[0].BindIP != ip {
		t.Fatalf("after bind: %+v", obs[0])
	}
	if awssdk.ToString(f.addrs["eipalloc-endpoint"].PrivateIpAddress) != "10.0.1.10" {
		t.Errorf("the endpoint's Elastic IP moved: %+v", f.addrs["eipalloc-endpoint"])
	}

	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Fatal(err)
	}
	if got := f.secondaries(); len(got) != 0 || f.tag() != "" {
		t.Errorf("after unbind: secondaries %v, tag %q", got, f.tag())
	}
	if f.addrs["eipalloc-endpoint"].AssociationId == nil {
		t.Error("unbinding took the endpoint's Elastic IP off too")
	}
	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Errorf("unbinding again: %v", err)
	}
}

// A failed association takes back the secondary address it assigned, so a retry doesn't pile them up.
func TestSecondaryBindFailureTakesTheAddressBack(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addInstance("i-a", "eni-a", "10.0.1.10")
	f.failAssociate = &smithy.GenericAPIError{Code: "UnauthorizedOperation"}
	p := New(f, secondaryPool("eipalloc-1"))
	obs, _ := p.Observe(ctx)
	if err := p.Bind(ctx, obs[0], "i-a"); err == nil {
		t.Fatal("bind succeeded")
	}
	if got := f.secondaries(); len(got) != 0 || f.tag() != "" {
		t.Errorf("left behind: secondaries %v, tag %q", got, f.tag())
	}
}

// Unbinding never takes back an address the tag doesn't name, nor a primary address a tag names.
func TestSecondaryUnbindLeavesUntaggedAddresses(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addInstance("i-a", "eni-a", "10.0.1.10")
	// Associated by hand with the node's own address, as a pool switched from Primary would find it.
	if err := New(f, pool("eipalloc-1")).Bind(ctx, addresspool.Address{ID: "eipalloc-1"}, "i-a"); err != nil {
		t.Fatal(err)
	}
	p := New(f, secondaryPool("eipalloc-1"))
	obs, _ := p.Observe(ctx)
	if obs[0].BindIP != "10.0.1.10" {
		t.Errorf("BindIP = %q, want the node's own address", obs[0].BindIP)
	}
	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "UnassignPrivateIpAddresses") {
			t.Errorf("unassigned an address the pool didn't assign: %v", f.calls)
		}
	}

	f.addrs["eipalloc-1"].Tags = []ec2types.Tag{{Key: awssdk.String(PrivateAddressTag), Value: awssdk.String("10.0.1.10")}}
	if err := p.Unbind(ctx, addresspool.Address{ID: "eipalloc-1"}); err == nil {
		t.Error("took back a primary address")
	}
}

func TestObserve(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing allocation ID is reported on its own", func(t *testing.T) {
		f := newFakeEC2()
		f.addEIP("eipalloc-1", "203.0.113.1")
		obs, err := New(f, pool("eipalloc-1", "eipalloc-gone")).Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if obs[0].Err != nil || obs[0].IP != "203.0.113.1" || obs[1].Err == nil {
			t.Fatalf("Observe = %+v", obs)
		}
	})

	t.Run("an address on an interface with no instance is bound to something that's not a node", func(t *testing.T) {
		f := newFakeEC2()
		f.addEIP("eipalloc-1", "203.0.113.1")
		f.addrs["eipalloc-1"].AssociationId = awssdk.String("eipassoc-nat")
		f.addrs["eipalloc-1"].NetworkInterfaceId = awssdk.String("eni-natgw")
		obs, _ := New(f, pool("eipalloc-1")).Observe(ctx)
		if obs[0].NodeID != "eni-natgw" {
			t.Fatalf("Observe = %+v", obs)
		}
	})

	t.Run("any other failure fails the read", func(t *testing.T) {
		f := newFakeEC2()
		f.failErr = errors.New("RequestLimitExceeded")
		if _, err := New(f, pool("eipalloc-1")).Observe(ctx); err == nil {
			t.Fatal("no error")
		}
	})
}

// The provider plugs into the shared planner: a whole replace-a-node pass, end to end through fake EC2.
func TestWithPlanner(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC2()
	f.addEIP("eipalloc-1", "203.0.113.1")
	f.addInstance("i-old", "eni-old", "10.0.1.10")
	f.addInstance("i-new", "eni-new", "10.0.1.20")
	p := New(f, pool("eipalloc-1"))
	claim := sipv1alpha1.AddressClaim{Kind: "KamailioCluster", Namespace: "telephony", Name: "proxy", Pod: "proxy-0"}

	run := func(nodes []addresspool.Node, node string, prev []sipv1alpha1.PoolAddress) []sipv1alpha1.PoolAddress {
		obs, err := p.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		plan := addresspool.Decide(addresspool.Input{Observed: obs, Nodes: nodes, Claimants: []addresspool.Claimant{{Claim: claim, Node: node}}, Previous: prev})
		for _, g := range plan.Steps {
			for _, s := range g {
				if s.Unbind {
					err = p.Unbind(ctx, s.Address)
				} else {
					err = p.Bind(ctx, s.Address, s.NodeID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		return plan.Status
	}
	st := run([]addresspool.Node{{Name: "old", ID: "i-old"}}, "old", nil)
	if st[0].State != sipv1alpha1.PoolAddressBound || awssdk.ToString(f.addrs["eipalloc-1"].InstanceId) != "i-old" {
		t.Fatalf("first pass: %+v", st)
	}
	st = run([]addresspool.Node{{Name: "new", ID: "i-new"}}, "new", st)
	if st[0].Node != "new" || awssdk.ToString(f.addrs["eipalloc-1"].InstanceId) != "i-new" {
		t.Fatalf("after replacement: %+v, EC2 has %v", st, awssdk.ToString(f.addrs["eipalloc-1"].InstanceId))
	}
}
