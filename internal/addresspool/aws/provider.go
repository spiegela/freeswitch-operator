// Package aws is the SIPAddressPool provider for AWS Elastic IPs. An Elastic IP is associated with a node's
// instance (its primary network interface's primary private address), and AWS translates 1:1, so the datapath is
// NAT; disassociating works whether or not the instance is alive, so fencing is authoritative.
//
// It needs ec2:DescribeAddresses, ec2:DescribeNetworkInterfaces, ec2:AssociateAddress and ec2:DisassociateAddress,
// through IRSA or EKS Pod Identity; the default credential chain finds either. A pool with privateAddress Secondary
// also needs ec2:AssignPrivateIpAddresses, ec2:UnassignPrivateIpAddresses, ec2:CreateTags and ec2:DeleteTags.
package aws

import (
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

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
	"git.instruments.com/ci/freeswitch-operator/internal/addresspool"
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

// Observe describes the pool's Elastic IPs in one call. When an allocation ID doesn't exist the whole call fails,
// so it then describes them one at a time, to report the missing one and still see the rest.
func (p *Provider) Observe(ctx context.Context) ([]addresspool.Address, error) {
	found := map[string]ec2types.Address{}
	failed := map[string]error{}
	out, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: p.ids})
	switch {
	case err == nil:
		for _, a := range out.Addresses {
			found[awssdk.ToString(a.AllocationId)] = a
		}
	case apiErrorCode(err) == "InvalidAllocationID.NotFound":
		for _, id := range p.ids {
			one, err := p.api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{id}})
			switch {
			case err != nil:
				failed[id] = err
			case len(one.Addresses) == 1:
				found[id] = one.Addresses[0]
			}
		}
	default:
		return nil, err
	}

	res := make([]addresspool.Address, 0, len(p.ids))
	for _, id := range p.ids {
		a, ok := found[id]
		if !ok {
			err := failed[id]
			if err == nil {
				err = fmt.Errorf("no Elastic IP with allocation ID %s in this account and region", id)
			}
			res = append(res, addresspool.Address{ID: id, Err: err})
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
// With privateAddress Secondary it instead assigns the interface a new secondary private address, records it on the
// Elastic IP (PrivateAddressTag) and associates the Elastic IP with it, leaving any Elastic IP on the node's own
// address alone. A failed association takes the secondary address back.
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
	assigned, err := p.api.AssignPrivateIpAddresses(ctx, &ec2.AssignPrivateIpAddressesInput{
		NetworkInterfaceId:             eni.NetworkInterfaceId,
		SecondaryPrivateIpAddressCount: awssdk.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("assigning a secondary private address to %s: %w", eniID, err)
	}
	if len(assigned.AssignedPrivateIpAddresses) != 1 {
		return fmt.Errorf("assigning a secondary private address to %s: AWS assigned %d", eniID, len(assigned.AssignedPrivateIpAddresses))
	}
	ip := awssdk.ToString(assigned.AssignedPrivateIpAddresses[0].PrivateIpAddress)
	tagged := false
	fail := func(step string, err error) error {
		err = fmt.Errorf("%s: %w", step, err)
		if uerr := p.unassign(ctx, eniID, ip); uerr != nil {
			err = fmt.Errorf("%w; taking back %s from %s also failed: %w", err, ip, eniID, uerr)
		}
		if !tagged {
			return err
		}
		if derr := p.untag(ctx, a.ID); derr != nil {
			err = fmt.Errorf("%w; removing the %s tag also failed: %w", err, PrivateAddressTag, derr)
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
	ip := ""
	for _, t := range cur.Tags {
		if awssdk.ToString(t.Key) == PrivateAddressTag {
			ip = awssdk.ToString(t.Value)
		}
	}
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
	return p.untag(ctx, a.ID)
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
