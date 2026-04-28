package main

import (
    "fmt"
    "encoding/json"
    "github.com/vishvananda/netlink"
    "github.com/containernetworking/plugins/pkg/ns"
    "github.com/containernetworking/cni/pkg/skel"
    "github.com/containernetworking/cni/pkg/types"
    current "github.com/containernetworking/cni/pkg/types/100"
    "github.com/containernetworking/cni/pkg/version"
)


type NetConf struct {
    CNIVersion string `json:"cniVersion"`
    Name       string `json:"name"`
    Type       string `json:"type"`
}


func cmdAdd(args *skel.CmdArgs) error {
    fmt.Println("ADD called for container:", args.ContainerID)
    conf := &NetConf{}
    
    if err := json.Unmarshal(args.StdinData, conf); err != nil{
        return fmt.Errorf("failed to parse CNI config: %w", err)
    }
    

    fmt.Println("Network name:", conf.Name)

    netns, err := ns.GetNS(args.Netns)
    if err != nil{
        return fmt.Errorf("failed to open netns %s: %w", args.Netns, err)
    }
    defer netns.Close()
    
    hostVethName := "veth" + args.ContainerID[:5]
    containerVeth := args.Ifname
    veth := &netlink.Veth{
	    Link.Atrrs: netlink.LinkAttrs{
		    Name: hostVethName,
	   },
	   Peer: containerVeth,
    }

    if err := netlink.LinkAdd(veth); err != nil{
        return    fmt.Errorf("failed to create veth pair: %w", err)
    }

    peer, err := netlink.LinkByName(containerVeth)
    if err != nil{
	    return fmt.Errorf("Failed to find containerVeth: %w", err)
    }

    if err := netlink.LinkSetnsFd(peer, int(netns.Fd())); err != nil{
	    return fmt.Errorf("Failed to move veth into netns: %w", err)
    }

    err = netns.Do(func(_ ns.NetNS) error {
        links, err := netlink.LinkByName(containerVeth)
	
        if err != nil {
            return err
        }

	if err := netlink.LinkSetup(link); err != nil{
	return err	
	}
	fmt.Println("Container intreface brought up:", containerVeth)
        return nil
    })
    if err != nil {
        return err
    }

    hostlink, err := netlink.LinkByName(hostVethname)
    if err != nil{
	    return fmt.Errorf("Failed to find the hostVethname: %w", err)
    }

    if err := netlink.LinkSetup(hostlink); err != nil {
	    return fmt.Errorf("Failed to bring host veth up: %w", err)
    }

    result := &current.Result{
        CNIVersion: conf.CNIVersion,
    }

    return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
    fmt.Println("DEL called for container:", args.ContainerID)
    return nil
}

func cmdCheck(args *skel.CmdArgs) error {
    fmt.Println("CHECK called")
    return nil
}

func main() {
    skel.PluginMain(
        cmdAdd,
        cmdCheck,
        cmdDel,
        version.All,
        "My Custom CNI",
    )
}

