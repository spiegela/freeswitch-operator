//go:build linux

package addressagent

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// Netlink is Links on the host's network namespace (the agent runs with host networking and NET_ADMIN).
type Netlink struct{}

func (Netlink) Interface(hostIP netip.Addr) (Link, []Addr, error) {
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return Link{}, nil, err
	}
	var link Link
	out := make([]Addr, 0, len(addrs))
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP.To4())
		if !ok {
			continue
		}
		out = append(out, Addr{IP: ip, Label: a.Label})
		if ip == hostIP {
			l, err := netlink.LinkByIndex(a.LinkIndex)
			if err != nil {
				return Link{}, nil, err
			}
			link = Link{Name: l.Attrs().Name, Index: a.LinkIndex}
		}
	}
	if link.Name == "" {
		return Link{}, nil, fmt.Errorf("no interface holds the node's address %s", hostIP)
	}
	return link, out, nil
}

// device names a link for the address calls, which only need its index.
func device(l Link) netlink.Link {
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: l.Name, Index: l.Index}}
}

func (Netlink) Add(l Link, ip netip.Addr, label string) error {
	return netlink.AddrAdd(device(l), &netlink.Addr{IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)}, Label: label})
}

func (Netlink) Del(l Link, ip netip.Addr) error {
	return netlink.AddrDel(device(l), &netlink.Addr{IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)}})
}
