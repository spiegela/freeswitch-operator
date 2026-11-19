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

func (Netlink) Interface(hostIP netip.Addr) (string, []Addr, error) {
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", nil, err
	}
	iface := ""
	out := make([]Addr, 0, len(addrs))
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP.To4())
		if !ok {
			continue
		}
		out = append(out, Addr{IP: ip, Label: a.Label})
		if ip == hostIP {
			link, err := netlink.LinkByIndex(a.LinkIndex)
			if err != nil {
				return "", nil, err
			}
			iface = link.Attrs().Name
		}
	}
	if iface == "" {
		return "", nil, fmt.Errorf("no interface holds the node's address %s", hostIP)
	}
	return iface, out, nil
}

func (Netlink) Add(iface string, ip netip.Addr, label string) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	return netlink.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)}, Label: label})
}

func (Netlink) Del(iface string, ip netip.Addr) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	return netlink.AddrDel(link, &netlink.Addr{IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)}})
}
