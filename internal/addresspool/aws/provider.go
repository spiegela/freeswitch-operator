// Package aws is the SIPAddressPool provider for AWS Elastic IPs. An Elastic IP is associated with a node's
// instance (its primary network interface's primary private address), and AWS translates 1:1, so the datapath is
// NAT; disassociating works whether or not the instance is alive, so fencing is authoritative.
//
// It needs ec2:DescribeAddresses, ec2:DescribeNetworkInterfaces, ec2:AssociateAddress and ec2:DisassociateAddress,
// through IRSA or EKS Pod Identity; the default credential chain finds either. A pool with privateAddress Secondary
// also needs ec2:AssignPrivateIpAddresses, ec2:UnassignPrivateIpAddresses, ec2:CreateTags and ec2:DeleteTags.
package aws

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

// EC2 is the part of the EC2 API the provider uses.
type EC2 interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	AssociateAddress(ctx context.Context, in *ec2.AssociateAddressInput, opts ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error)
	DisassociateAddress(ctx context.Context, in *ec2.DisassociateAddressInput, opts ...func(*ec2.Options)) (*ec2.DisassociateAddressOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, opts ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	AssignPrivateIpAddresses(ctx context.Context, in *ec2.AssignPrivateIpAddressesInput, opts ...func(*ec2.Options)) (*ec2.AssignPrivateIpAddressesOutput, error)
	UnassignPrivateIpAddresses(ctx context.Context, in *ec2.UnassignPrivateIpAddressesInput, opts ...func(*ec2.Options)) (*ec2.UnassignPrivateIpAddressesOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
}

// PrivateAddressTag is set on an Elastic IP to the secondary private address the provider assigned for it, so that
// unbinding takes back only an address the operator assigned, never one something else put on the interface.
const PrivateAddressTag = "sip.instruments.com/private-address"

// LastPrivateAddressTag is set on an Elastic IP to the secondary private address unbinding took back, and binding
// asks for that address again. Pods bind their media to it (a pool's bind address), and FreeSWITCH binds a recovered
// call's media to the address the call had: with the same address on the next node, calls recovered there keep
// their media. AWS gives it only when the next node's interface is in the same subnet and nothing has taken the
// address since; otherwise the Elastic IP gets a new one.
const LastPrivateAddressTag = "sip.instruments.com/last-private-address"

// Provider binds one pool's Elastic IPs.
type Provider struct {
	api       EC2
	ids       []string
	secondary bool
}

// New returns the provider for a pool's Elastic IPs, through api.
func New(api EC2, spec *sipv1alpha1.AWSElasticIPPool) *Provider {
	return &Provider{api: api, ids: slices.Clone(spec.AllocationIDs), secondary: spec.PrivateAddress == sipv1alpha1.AWSPrivateAddressSecondary}
}

// NewFactory loads AWS configuration once and returns a Factory that gives each pool a client for its region.
func NewFactory(ctx context.Context) (addresspool.Factory, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	var mu sync.Mutex
	clients := map[string]*ec2.Client{}
	return func(_ context.Context, pool *sipv1alpha1.SIPAddressPool) (addresspool.Provider, error) {
		spec := pool.Spec.AWSElasticIP
		if spec == nil {
			return nil, errors.New("not an Elastic IP pool")
		}
		mu.Lock()
		defer mu.Unlock()
		c, ok := clients[spec.Region]
		if !ok {
			c = ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.Region = spec.Region })
			clients[spec.Region] = c
		}
		return New(c, spec), nil
	}, nil
}

func (p *Provider) Capabilities() addresspool.Capabilities {
	return addresspool.Capabilities{Datapath: sipv1alpha1.AddressDatapathNAT, Fencing: sipv1alpha1.AddressFencingAuthoritative}
}

// NodeID is the node's instance ID, from its provider ID (aws:///<zone>/<instance>).
func (p *Provider) NodeID(n *corev1.Node) (string, error) {
	id := n.Spec.ProviderID
	if !strings.HasPrefix(id, "aws://") {
		return "", fmt.Errorf("provider ID %q is not an AWS one", id)
	}
	instance := id[strings.LastIndex(id, "/")+1:]
	if !strings.HasPrefix(instance, "i-") {
		return "", fmt.Errorf("provider ID %q names no instance", id)
	}
	return instance, nil
}

// filterValuesMax is the most values EC2 takes in one filter; a pool may list more allocation IDs.
const filterValuesMax = 200

// Observe describes the pool's Elastic IPs, in one call for up to filterValuesMax of them. It filters by allocation
// ID rather than naming them, since naming one that doesn't exist fails the whole call; filtered, a missing one is
// just absent, and reported on its own.
func (p *Provider) Observe(ctx context.Context) ([]addresspool.Address, error) {
	found := map[string]ec2types.Address{}
	for ids := range slices.Chunk(p.ids, filterValuesMax) {
		out, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
			Filters: []ec2types.Filter{{Name: awssdk.String("allocation-id"), Values: ids}},
		})
		if err != nil {
			return nil, err
		}
		for _, a := range out.Addresses {
			found[awssdk.ToString(a.AllocationId)] = a
		}
	}

	res := make([]addresspool.Address, 0, len(p.ids))
	for _, id := range p.ids {
		a, ok := found[id]
		if !ok {
			res = append(res, addresspool.Address{ID: id, Err: fmt.Errorf("no Elastic IP with allocation ID %s in this account and region", id)})
			continue
		}
		obs := addresspool.Address{ID: id, IP: awssdk.ToString(a.PublicIp), BindingID: awssdk.ToString(a.AssociationId)}
		switch {
		case a.InstanceId != nil:
			obs.NodeID = *a.InstanceId
			if p.secondary {
				// Pods bind whatever private address the Elastic IP translates to, even the node's own (a pool
				// switched from Primary): the address agent finds that one already on the interface.
				obs.BindIP = awssdk.ToString(a.PrivateIpAddress)
			}
		case a.NetworkInterfaceId != nil:
			// Associated with an interface that belongs to no instance (a NAT gateway, a load balancer): never a
			// node, so the pool reports a conflict.
			obs.NodeID = *a.NetworkInterfaceId
		case obs.BindingID != "":
			obs.NodeID = obs.BindingID
		}
		res = append(res, obs)
	}
	return res, nil
}

// Bind associates the Elastic IP with the primary private address of the instance's primary network interface,
// the node's own address. It names the interface because AWS refuses an instance ID alone once the instance has a
// second interface, which EKS nodes with the VPC CNI usually do. It refuses to replace an Elastic IP the instance
// already holds that isn't the pool's, and (AllowReassociation false) to take the address from anything else.
//
// With privateAddress Secondary it instead assigns the interface a secondary private address (the one the Elastic
// IP had last, when AWS gives it; else a new one), records it on the Elastic IP (PrivateAddressTag) and associates
// the Elastic IP with it, leaving any Elastic IP on the node's own address alone. A failed association takes the
// secondary address back and puts the tag back as it was.
func (p *Provider) Bind(ctx context.Context, a addresspool.Address, instanceID string) error {
	if p.secondary {
		return p.bindSecondary(ctx, a, instanceID)
	}
	held, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []ec2types.Filter{{Name: awssdk.String("instance-id"), Values: []string{instanceID}}},
	})
	if err != nil {
		return err
	}
	for _, h := range held.Addresses {
		if id := awssdk.ToString(h.AllocationId); !slices.Contains(p.ids, id) {
			return fmt.Errorf("instance %s already holds Elastic IP %s (%s), which is not in the pool", instanceID, awssdk.ToString(h.PublicIp), id)
		}
	}
	eni, err := p.primaryInterface(ctx, instanceID)
	if err != nil {
		return err
	}
	_, err = p.api.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId:       awssdk.String(a.ID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
		PrivateIpAddress:   eni.PrivateIpAddress,
		AllowReassociation: awssdk.Bool(false),
	})
	return err
}

// Unbind disassociates the Elastic IP. One already disassociated is no error. With privateAddress Secondary it
// then takes back the secondary private address it assigned for the Elastic IP, if any.
func (p *Provider) Unbind(ctx context.Context, a addresspool.Address) error {
	if p.secondary {
		return p.unbindSecondary(ctx, a)
	}
	return p.disassociate(ctx, a.BindingID)
}

func (p *Provider) disassociate(ctx context.Context, associationID string) error {
	if associationID == "" {
		return nil
	}
	_, err := p.api.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: awssdk.String(associationID)})
	if apiErrorCode(err) == "InvalidAssociationID.NotFound" {
		return nil
	}
	return err
}

// primaryInterface is the instance's network interface at device index 0.
func (p *Provider) primaryInterface(ctx context.Context, instanceID string) (ec2types.NetworkInterface, error) {
	enis, err := p.api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{
			{Name: awssdk.String("attachment.instance-id"), Values: []string{instanceID}},
			{Name: awssdk.String("attachment.device-index"), Values: []string{"0"}},
		},
	})
	if err != nil {
		return ec2types.NetworkInterface{}, err
	}
	if len(enis.NetworkInterfaces) != 1 {
		return ec2types.NetworkInterface{}, fmt.Errorf("instance %s has %d primary network interfaces, want 1", instanceID, len(enis.NetworkInterfaces))
	}
	return enis.NetworkInterfaces[0], nil
}

func (p *Provider) bindSecondary(ctx context.Context, a addresspool.Address, instanceID string) error {
	eni, err := p.primaryInterface(ctx, instanceID)
	if err != nil {
		return err
	}
	eniID := awssdk.ToString(eni.NetworkInterfaceId)
	out, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{a.ID}})
	if err != nil {
		return err
	}
	if len(out.Addresses) != 1 {
		return fmt.Errorf("no Elastic IP with allocation ID %s", a.ID)
	}
	cur := out.Addresses[0]
	if cur.AssociationId != nil {
		// Refused before touching anything: the association below would fail anyway (AllowReassociation false), and
		// overwriting the tag first would lose the private address its binding really has. A read that missed a
		// binding just made (EC2 is eventually consistent) gets here.
		return fmt.Errorf("the Elastic IP %s is already associated (%s)", a.ID, awssdk.ToString(cur.AssociationId))
	}
	// A binding that ended without Unbind (AWS disassociates a terminated instance's Elastic IP itself) still names its
	// private address in PrivateAddressTag; Unbind moves it to LastPrivateAddressTag.
	prevTag := tagValue(cur.Tags, PrivateAddressTag)
	ip, err := p.assignSecondary(ctx, eniID, cmp.Or(prevTag, tagValue(cur.Tags, LastPrivateAddressTag)))
	if err != nil {
		return fmt.Errorf("assigning a secondary private address to %s: %w", eniID, err)
	}
	tagged := false
	fail := func(step string, err error) error {
		err = fmt.Errorf("%s: %w", step, err)
		if uerr := p.unassign(ctx, eniID, ip); uerr != nil {
			err = fmt.Errorf("%w; taking back %s from %s also failed: %w", err, ip, eniID, uerr)
		}
		if !tagged {
			return err
		}
		if rerr := p.restoreTag(ctx, a.ID, prevTag); rerr != nil {
			err = fmt.Errorf("%w; restoring the %s tag also failed: %w", err, PrivateAddressTag, rerr)
		}
		return err
	}
	if _, err := p.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.ID},
		Tags:      []ec2types.Tag{{Key: awssdk.String(PrivateAddressTag), Value: awssdk.String(ip)}},
	}); err != nil {
		return fail("recording "+ip+" on the Elastic IP", err)
	}
	tagged = true
	if _, err := p.api.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId:       awssdk.String(a.ID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
		PrivateIpAddress:   awssdk.String(ip),
		AllowReassociation: awssdk.Bool(false),
	}); err != nil {
		return fail("associating with "+ip, err)
	}
	return nil
}

// unbindSecondary disassociates the Elastic IP, then takes back the secondary private address its tag names, from
// whichever interface still has it. An address the tag doesn't name is never taken back.
func (p *Provider) unbindSecondary(ctx context.Context, a addresspool.Address) error {
	out, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{a.ID}})
	if err != nil {
		return err
	}
	if len(out.Addresses) != 1 {
		return fmt.Errorf("no Elastic IP with allocation ID %s", a.ID)
	}
	cur := out.Addresses[0]
	if err := p.disassociate(ctx, awssdk.ToString(cur.AssociationId)); err != nil {
		return err
	}
	ip := tagValue(cur.Tags, PrivateAddressTag)
	if ip == "" {
		return nil
	}
	enis, err := p.api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: awssdk.String("addresses.private-ip-address"), Values: []string{ip}}},
	})
	if err != nil {
		return err
	}
	for _, eni := range enis.NetworkInterfaces {
		for _, pa := range eni.PrivateIpAddresses {
			if awssdk.ToString(pa.PrivateIpAddress) != ip {
				continue
			}
			if awssdk.ToBool(pa.Primary) {
				return fmt.Errorf("%s is the primary private address of %s, not one the pool assigned", ip, awssdk.ToString(eni.NetworkInterfaceId))
			}
			if err := p.unassign(ctx, awssdk.ToString(eni.NetworkInterfaceId), ip); err != nil {
				return err
			}
		}
	}
	// Remembered before the tag that lets unbinding take it back goes: binding asks for it again. Best effort: without
	// it (a role that may only set the older tag, say) binding just takes a new address, as it always did.
	_, _ = p.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.ID},
		Tags:      []ec2types.Tag{{Key: awssdk.String(LastPrivateAddressTag), Value: awssdk.String(ip)}},
	})
	return p.untag(ctx, a.ID)
}

// assignSecondary assigns the interface one secondary private address and returns it: last, when it is set and AWS
// gives it (the interface is in its subnet, and nothing holds it), else any free one.
func (p *Provider) assignSecondary(ctx context.Context, eniID, last string) (string, error) {
	assign := func(in *ec2.AssignPrivateIpAddressesInput) (string, error) {
		in.NetworkInterfaceId = awssdk.String(eniID)
		out, err := p.api.AssignPrivateIpAddresses(ctx, in)
		if err != nil {
			return "", err
		}
		if len(out.AssignedPrivateIpAddresses) != 1 {
			return "", fmt.Errorf("AWS assigned %d addresses", len(out.AssignedPrivateIpAddresses))
		}
		return awssdk.ToString(out.AssignedPrivateIpAddresses[0].PrivateIpAddress), nil
	}
	var lastErr error
	if last != "" {
		// Never AllowReassignment: an address something else has taken since (the VPC CNI, for a pod) stays theirs.
		ip, err := assign(&ec2.AssignPrivateIpAddressesInput{PrivateIpAddresses: []string{last}})
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	ip, err := assign(&ec2.AssignPrivateIpAddressesInput{SecondaryPrivateIpAddressCount: awssdk.Int32(1)})
	if err != nil && lastErr != nil {
		err = fmt.Errorf("%w (and asking for %s again: %w)", err, last, lastErr)
	}
	return ip, err
}

// tagValue is the value of the tag with key, or "".
func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if awssdk.ToString(t.Key) == key {
			return awssdk.ToString(t.Value)
		}
	}
	return ""
}

func (p *Provider) unassign(ctx context.Context, eniID, ip string) error {
	_, err := p.api.UnassignPrivateIpAddresses(ctx, &ec2.UnassignPrivateIpAddressesInput{
		NetworkInterfaceId: awssdk.String(eniID),
		PrivateIpAddresses: []string{ip},
	})
	if apiErrorCode(err) == "InvalidNetworkInterfaceID.NotFound" {
		// The interface has gone with its instance, and the address with it.
		return nil
	}
	return err
}

// restoreTag sets PrivateAddressTag back to prev, or removes it when there was none.
func (p *Provider) restoreTag(ctx context.Context, allocationID, prev string) error {
	if prev == "" {
		return p.untag(ctx, allocationID)
	}
	_, err := p.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{allocationID},
		Tags:      []ec2types.Tag{{Key: awssdk.String(PrivateAddressTag), Value: awssdk.String(prev)}},
	})
	return err
}

func (p *Provider) untag(ctx context.Context, allocationID string) error {
	_, err := p.api.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{allocationID},
		Tags:      []ec2types.Tag{{Key: awssdk.String(PrivateAddressTag)}},
	})
	return err
}

func apiErrorCode(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorCode()
	}
	return ""
}
