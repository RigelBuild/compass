//go:build linux

package guestd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

const (
	// defaultNetIface is the virtio-net interface passt's link is exposed as
	// inside the guest. cloud-hypervisor presents the single virtio-net device
	// as eth0.
	defaultNetIface = "eth0"
	// loopbackIface is brought up so the guest's own loopback works before the
	// external interface (§(d) step 2).
	loopbackIface = "lo"
	// resolvConfPath is where the lease-derived resolver config is written, the
	// file the arm step reads (§(c), egress).
	resolvConfPath = "/etc/resolv.conf"
)

// linuxNetProvisioner is the real netProvisioner: it links up lo and the
// virtio-net interface, runs an in-process DHCP client (OQ-C) to acquire a
// lease, installs the address and default route, and writes /etc/resolv.conf
// (§(d) step 2). It is Linux-only and touches netlink + a raw DHCP socket, so it
// runs only inside the VM (proven by T4); the derivation it applies is unit-
// tested via leaseToConfig / renderResolvConf.
type linuxNetProvisioner struct {
	iface string
	log   *slog.Logger
}

// Provision brings networking up end to end, fail-closed at every step.
func (p *linuxNetProvisioner) Provision(ctx context.Context) error {
	if err := linkUp(loopbackIface); err != nil {
		return fmt.Errorf("bringing up %s: %w", loopbackIface, err)
	}
	if err := linkUp(p.iface); err != nil {
		return fmt.Errorf("bringing up %s: %w", p.iface, err)
	}

	lease, err := acquireLease(ctx, p.iface)
	if err != nil {
		return fmt.Errorf("acquiring DHCP lease on %s: %w", p.iface, err)
	}
	cfg, err := leaseToConfig(lease.ACK)
	if err != nil {
		return fmt.Errorf("deriving network config from lease: %w", err)
	}

	if err := applyNetConfig(p.iface, cfg); err != nil {
		return err
	}
	if p.log != nil {
		p.log.Info("network provisioned",
			slog.String("iface", p.iface),
			slog.String("addr", cfg.addr.String()),
			slog.Any("gateway", cfg.gateway),
			slog.Any("dns", cfg.dns))
	}
	return nil
}

// linkUp resolves an interface by name and sets it administratively up.
func linkUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("resolving link: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("setting link up: %w", err)
	}
	return nil
}

// acquireLease runs one DORA exchange on iface against passt's built-in DHCP
// server and returns the lease.
func acquireLease(ctx context.Context, iface string) (*nclient4.Lease, error) {
	client, err := nclient4.New(iface)
	if err != nil {
		return nil, fmt.Errorf("creating DHCP client: %w", err)
	}
	defer client.Close() //nolint:errcheck // teardown of a client whose lease is already captured

	lease, err := client.Request(ctx)
	if err != nil {
		return nil, fmt.Errorf("DHCP request: %w", err)
	}
	return lease, nil
}

// applyNetConfig installs the derived address, default route, and resolv.conf on
// iface. It is the netlink/filesystem side of the lease application; the
// derivation is leaseToConfig's pure job.
func applyNetConfig(iface string, cfg netConfig) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("resolving link %s: %w", iface, err)
	}

	addr := &netlink.Addr{IPNet: &net.IPNet{IP: cfg.addr.IP, Mask: cfg.addr.Mask}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("adding address %s to %s: %w", cfg.addr.String(), iface, err)
	}

	if cfg.gateway != nil {
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: cfg.gateway}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("installing default route via %s: %w", cfg.gateway, err)
		}
	}

	if err := os.WriteFile(resolvConfPath, []byte(renderResolvConf(cfg.dns, cfg.searchDomains...)), 0o644); err != nil { //nolint:gosec // resolv.conf is world-readable by contract
		return fmt.Errorf("writing %s: %w", resolvConfPath, err)
	}
	return nil
}

// compile-time assertion that the real implementation satisfies the seam.
var _ netProvisioner = (*linuxNetProvisioner)(nil)
