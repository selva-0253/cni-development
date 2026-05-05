# my-cni deployment notes

`my-cni` wires pods on the local node. `my-cni-agent` adds the missing
cross-node routes.

Build both binaries for Linux:

```sh
GOOS=linux GOARCH=amd64 go build -o my-cni ./cmd/my-cni
GOOS=linux GOARCH=amd64 go build -o my-cni-agent ./cmd/my-cni-agent
```

Install the CNI binary:

```sh
sudo install -m 0755 my-cni /opt/cni/bin/my-cni
```

On each node, create `/etc/cni/net.d/my-cni-routes.json`. Use the same
`nodes` list everywhere, but set `nodeName` to the current node.

For GCP routed networking, set `gateway` to the subnet gateway, for example
`10.0.0.1`. GCP VMs do not ARP each other directly, so routes should go via the
subnet gateway while GCP VPC routes deliver pod CIDRs to the right instance.

Run the route agent as root:

```sh
sudo ./my-cni-agent -config /etc/cni/net.d/my-cni-routes.json
```

For a two-node cluster, the agent creates routes like:

```text
10.244.2.0/24 via 10.0.0.1 dev ens4
10.244.1.0/24 via 10.0.0.1 dev ens4
```

This is the routed-network learning step before adding a Kubernetes API watcher
or VXLAN overlay.
