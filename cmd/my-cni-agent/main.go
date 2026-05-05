package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/vishvananda/netlink"
)

const defaultConfigPath = "/etc/cni/net.d/my-cni-routes.json"

type Config struct {
	NodeName string `json:"nodeName"`
	Gateway  string `json:"gateway"`
	Nodes    []Node `json:"nodes"`
}

type Node struct {
	Name       string `json:"name"`
	PodCIDR    string `json:"podCIDR"`
	InternalIP string `json:"internalIP"`
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "route config file")
	interval := flag.Duration("interval", 30*time.Second, "route reconciliation interval")
	once := flag.Bool("once", false, "run one reconciliation and exit")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	for {
		if err := reconcile(*configPath); err != nil {
			log.Printf("reconcile failed: %v", err)
		}

		if *once {
			return
		}
		time.Sleep(*interval)
	}
}

func reconcile(configPath string) error {
	conf, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	if err := enableIPForwarding(); err != nil {
		log.Printf("warning: could not enable ip_forward: %v", err)
	}

	localNode := conf.NodeName
	if localNode == "" {
		localNode = os.Getenv("NODE_NAME")
	}
	if localNode == "" {
		localNode, _ = os.Hostname()
	}

	localIPs := hostIPv4s()
	for _, node := range conf.Nodes {
		if err := validateNode(node); err != nil {
			log.Printf("skipping invalid node route entry: %v", err)
			continue
		}
		if isLocalNode(node, localNode, localIPs) {
			continue
		}
		if err := ensureRoute(node, conf.Gateway); err != nil {
			log.Printf("failed to install route for node %s: %v", node.Name, err)
			continue
		}
		log.Printf("route ready: %s via %s (%s)", node.PodCIDR, node.InternalIP, node.Name)
	}

	return nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	conf := &Config{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(conf.Nodes) == 0 {
		return nil, fmt.Errorf("config %s has no nodes", path)
	}
	return conf, nil
}

func validateNode(node Node) error {
	if node.Name == "" {
		return fmt.Errorf("missing node name")
	}
	if node.PodCIDR == "" {
		return fmt.Errorf("node %s missing podCIDR", node.Name)
	}
	if node.InternalIP == "" {
		return fmt.Errorf("node %s missing internalIP", node.Name)
	}
	if _, _, err := net.ParseCIDR(node.PodCIDR); err != nil {
		return fmt.Errorf("node %s has invalid podCIDR %q: %w", node.Name, node.PodCIDR, err)
	}
	if ip := net.ParseIP(node.InternalIP); ip == nil || ip.To4() == nil {
		return fmt.Errorf("node %s has invalid IPv4 internalIP %q", node.Name, node.InternalIP)
	}
	return nil
}

func isLocalNode(node Node, localNode string, localIPs map[string]struct{}) bool {
	if localNode != "" && node.Name == localNode {
		return true
	}
	_, ok := localIPs[node.InternalIP]
	return ok
}

func hostIPv4s() map[string]struct{} {
	ips := map[string]struct{}{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip.To4() == nil {
			continue
		}
		ips[ip.String()] = struct{}{}
	}
	return ips
}

func ensureRoute(node Node, gateway string) error {
	_, dst, err := net.ParseCIDR(node.PodCIDR)
	if err != nil {
		return err
	}

	linkIndex, err := defaultRouteLinkIndex()
	if err != nil {
		return err
	}

	nextHop := node.InternalIP
	flags := int(netlink.FLAG_ONLINK)
	if gateway != "" {
		nextHop = gateway
		flags = 0
	}

	gw := net.ParseIP(nextHop).To4()
	if gw == nil {
		return fmt.Errorf("invalid next hop %q", nextHop)
	}
	route := netlink.Route{
		LinkIndex: linkIndex,
		Dst:       dst,
		Gw:        gw,
		Flags:     flags,
	}
	if err := netlink.RouteReplace(&route); err != nil {
		return fmt.Errorf("route replace %s via %s: %w", dst, gw, err)
	}
	return nil
}

func defaultRouteLinkIndex() (int, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return 0, fmt.Errorf("list routes: %w", err)
	}
	for _, route := range routes {
		if route.Dst == nil && route.LinkIndex != 0 {
			return route.LinkIndex, nil
		}
	}
	return 0, fmt.Errorf("default IPv4 route not found")
}

func enableIPForwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}
