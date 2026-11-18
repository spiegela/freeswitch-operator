// Package aws is the SIPAddressPool provider for AWS Elastic IPs. An Elastic IP is associated with a node's
// instance (its primary network interface's primary private address), and AWS translates 1:1, so the datapath is
// NAT; disassociating works whether or not the instance is alive, so fencing is authoritative.
//
// It needs ec2:DescribeAddresses, ec2:DescribeNetworkInterfaces, ec2:AssociateAddress and ec2:DisassociateAddress,
// through IRSA or EKS Pod Identity; the default credential chain finds either.
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
}

// Provider binds one pool's Elastic IPs.
type Provider struct {
	api EC2
	ids []string
}

// New returns the provider for a pool's Elastic IPs, through api.
func New(api EC2, spec *sipv1alpha1.AWSElasticIPPool) *Provider {
	return &Provider{api: api, ids: slices.Clone(spec.AllocationIDs)}
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
func (p *Provider) Bind(ctx context.Context, a addresspool.Address, instanceID string) error {
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
	enis, err := p.api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{
			{Name: awssdk.String("attachment.instance-id"), Values: []string{instanceID}},
			{Name: awssdk.String("attachment.device-index"), Values: []string{"0"}},
		},
	})
	if err != nil {
		return err
	}
	if len(enis.NetworkInterfaces) != 1 {
		return fmt.Errorf("instance %s has %d primary network interfaces, want 1", instanceID, len(enis.NetworkInterfaces))
	}
	eni := enis.NetworkInterfaces[0]
	_, err = p.api.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId:       awssdk.String(a.ID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
		PrivateIpAddress:   eni.PrivateIpAddress,
		AllowReassociation: awssdk.Bool(false),
	})
	return err
}

// Unbind disassociates the Elastic IP. One already disassociated is no error.
func (p *Provider) Unbind(ctx context.Context, a addresspool.Address) error {
	if a.BindingID == "" {
		return nil
	}
	_, err := p.api.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: awssdk.String(a.BindingID)})
	if apiErrorCode(err) == "InvalidAssociationID.NotFound" {
		return nil
	}
	return err
}

func apiErrorCode(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorCode()
	}
	return ""
}
