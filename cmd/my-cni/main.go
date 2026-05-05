package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

type NetConf struct {
	types.NetConf        // embeds CNIVersion, Name, Type
	Bridge        string `json:"bridge"`
	IPAM          struct {
		Type   string `json:"type"`
		Subnet string `json:"subnet"`
	} `json:"ipam"`
}

func loadConf(data []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("failed to parse CNI config: %w", err)
	}
	if conf.Bridge == "" {
		conf.Bridge = "cni0"
	}
	return conf, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// init – lock OS thread so all netns calls stay on one thread
// ─────────────────────────────────────────────────────────────────────────────

func init() {
	runtime.LockOSThread()
}

// ─────────────────────────────────────────────────────────────────────────────
// Bridge helpers
// ─────────────────────────────────────────────────────────────────────────────

func ensureBridge(name string) (netlink.Link, error) {
	br, err := netlink.LinkByName(name)
	if err == nil {
		if err := netlink.LinkSetUp(br); err != nil {
			return nil, fmt.Errorf("failed to bring bridge %s up: %w", name, err)
		}
		return br, nil
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name:   name,
			TxQLen: 1000,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, fmt.Errorf("failed to create bridge %s: %w", name, err)
	}

	br, err = netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("bridge %s not found after creation: %w", name, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return nil, fmt.Errorf("failed to bring bridge %s up: %w", name, err)
	}
	return br, nil
}

// setBridgeGatewayIP assigns the gateway IP to the bridge (idempotent).
func setBridgeGatewayIP(br netlink.Link, gw net.IP, mask net.IPMask) error {
	addrs, err := netlink.AddrList(br, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("failed to list bridge addresses: %w", err)
	}
	for _, a := range addrs {
		if a.IP.Equal(gw) {
			return nil
		}
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: gw, Mask: mask},
	}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return fmt.Errorf("failed to add gateway IP %s to bridge: %w", gw, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// veth helpers
// ─────────────────────────────────────────────────────────────────────────────

// vethHostName derives a ≤15-char name from the container ID.
func vethHostName(containerID string) string {
	id := containerID
	if len(id) > 8 {
		id = id[:8]
	}
	return "veth" + id
}

// setupVeth creates the veth pair and moves the container peer into
// containerNS, renaming it to ifName and bringing it UP.
// Returns the host-side link (still in host netns, not yet UP).
func setupVeth(containerNS ns.NetNS, ifName, hvName string) (netlink.Link, error) {
	tmpPeer := "tmp" + hvName

	// Clean up any stale interface from a previous failed ADD.
	if stale, err := netlink.LinkByName(hvName); err == nil {
		_ = netlink.LinkDel(stale)
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hvName},
		PeerName:  tmpPeer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("failed to create veth pair %s<->%s: %w", hvName, tmpPeer, err)
	}

	hostLink, err := netlink.LinkByName(hvName)
	if err != nil {
		return nil, fmt.Errorf("host veth %s not found after creation: %w", hvName, err)
	}
	peerLink, err := netlink.LinkByName(tmpPeer)
	if err != nil {
		return nil, fmt.Errorf("container peer %s not found after creation: %w", tmpPeer, err)
	}

	// Move container peer into the container netns.
	if err := netlink.LinkSetNsFd(peerLink, int(containerNS.Fd())); err != nil {
		return nil, fmt.Errorf("failed to move peer into container netns: %w", err)
	}

	// Inside the container netns: rename and bring up.
	err = containerNS.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(tmpPeer)
		if err != nil {
			return fmt.Errorf("peer %s not found inside netns: %w", tmpPeer, err)
		}
		if err := netlink.LinkSetName(link, ifName); err != nil {
			return fmt.Errorf("failed to rename %s → %s: %w", tmpPeer, ifName, err)
		}
		link, err = netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("interface %s not found after rename: %w", ifName, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to bring %s up: %w", ifName, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return hostLink, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Sysctl
// ─────────────────────────────────────────────────────────────────────────────

func enableIPForwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

// ─────────────────────────────────────────────────────────────────────────────
// cmdAdd
// ─────────────────────────────────────────────────────────────────────────────

func cmdAdd(args *skel.CmdArgs) error {
	// 1. Parse config.
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	// 2. IPAM: allocate IP.
	ipamRaw, err := ipam.ExecAdd(conf.IPAM.Type, args.StdinData)
	if err != nil {
		return fmt.Errorf("IPAM ExecAdd failed: %w", err)
	}
	result, err := current.NewResultFromResult(ipamRaw)
	if err != nil {
		return fmt.Errorf("failed to convert IPAM result: %w", err)
	}
	if len(result.IPs) == 0 {
		return fmt.Errorf("IPAM returned no IPs")
	}

	allocIP := result.IPs[0]  // e.g. {Address: 10.10.0.2/16, Gateway: 10.10.0.1}
	gwIP    := allocIP.Gateway

	// 3. Ensure bridge exists and is up.
	br, err := ensureBridge(conf.Bridge)
	if err != nil {
		return err
	}

	// 4. Open container netns.
	containerNS, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %s: %w", args.Netns, err)
	}
	defer containerNS.Close()

	// 5. Create veth pair; move container peer into netns, rename, bring up.
	hvName   := vethHostName(args.ContainerID)
	hostLink, err := setupVeth(containerNS, args.IfName, hvName)
	if err != nil {
		return err
	}

	// 6. Inside container netns: assign IP + add default route.
	err = containerNS.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(args.IfName)
		if err != nil {
			return fmt.Errorf("container interface %s not found: %w", args.IfName, err)
		}

		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: &allocIP.Address}); err != nil {
			return fmt.Errorf("failed to assign IP %s: %w", allocIP.Address, err)
		}

		if gwIP != nil {
			route := &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       nil, // nil = 0.0.0.0/0 (default route)
				Gw:        gwIP,
			}
			if err := netlink.RouteAdd(route); err != nil {
				return fmt.Errorf("failed to add default route via %s: %w", gwIP, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 7. Host: bring host veth up and attach to bridge.
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return fmt.Errorf("failed to bring host veth %s up: %w", hvName, err)
	}
	if err := netlink.LinkSetMaster(hostLink, br); err != nil {
		return fmt.Errorf("failed to attach %s to bridge %s: %w", hvName, conf.Bridge, err)
	}

	// 8. Assign gateway IP to bridge (idempotent — only needed once).
	if gwIP != nil {
		if err := setBridgeGatewayIP(br, gwIP, allocIP.Address.Mask); err != nil {
			return err
		}
	}

	// 9. Enable IP forwarding on the host (idempotent).
	if err := enableIPForwarding(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not enable ip_forward: %v\n", err)
	}

	// 10. Build CNI result.
	result.Interfaces = []*current.Interface{
		{
			Name: conf.Bridge,
			Mac:  br.Attrs().HardwareAddr.String(),
		},
		{
			Name: hvName,
			Mac:  hostLink.Attrs().HardwareAddr.String(),
		},
		{
			Name:    args.IfName,
			Sandbox: args.Netns,
		},
	}
	containerIfIdx := 2
	for _, ip := range result.IPs {
		ip.Interface = &containerIfIdx
	}

	return types.PrintResult(result, conf.CNIVersion)
}

// ─────────────────────────────────────────────────────────────────────────────
// cmdDel
// ─────────────────────────────────────────────────────────────────────────────

func cmdDel(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	// Release the IP back to IPAM.
	if err := ipam.ExecDel(conf.IPAM.Type, args.StdinData); err != nil {
		fmt.Fprintf(os.Stderr, "warning: IPAM ExecDel error: %v\n", err)
	}

	// Delete the host-side veth (kernel auto-deletes the container peer).
	hvName := vethHostName(args.ContainerID)
	link, err := netlink.LinkByName(hvName)
	if err != nil {
		// Already gone — nothing to do.
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete host veth %s: %w", hvName, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// cmdCheck
// ─────────────────────────────────────────────────────────────────────────────

func cmdCheck(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	hvName := vethHostName(args.ContainerID)
	if _, err := netlink.LinkByName(hvName); err != nil {
		return fmt.Errorf("CHECK: host veth %s missing", hvName)
	}
	if _, err := netlink.LinkByName(conf.Bridge); err != nil {
		return fmt.Errorf("CHECK: bridge %s missing", conf.Bridge)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "my-cni v0.1.0")
}
