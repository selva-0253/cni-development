# GCP New Cluster Checklist

This checklist explains how to use `my-cni` for cross-node pod networking on a
new self-managed Kubernetes cluster in GCP.

The working model is:

```text
pod on node A
  -> cni0
  -> ens4
  -> GCP subnet gateway
  -> GCP VPC custom route
  -> node B
  -> cni0
  -> pod on node B
```

## 1. Assign one pod CIDR per node

Each worker node must have a unique pod CIDR.

Example:

```text
c1-wn1 -> 10.10.1.0/24
c1-wn2 -> 10.10.2.0/24
c1-wn3 -> 10.10.3.0/24
```

Do not use the same IPAM subnet on every node. Each node's CNI config should use
that node's own pod CIDR.

## 2. Build and install the CNI binary

Build on your development machine or directly on a Linux node:

```sh
GOOS=linux GOARCH=amd64 go build -o my-cni ./cmd/my-cni
```

Install on every worker node:

```sh
sudo install -m 0755 my-cni /opt/cni/bin/my-cni
```

## 3. Build and install the route agent

Build:

```sh
GOOS=linux GOARCH=amd64 go build -o my-cni-agent ./cmd/my-cni-agent
```

Install on every worker node:

```sh
sudo install -m 0755 my-cni-agent /usr/local/bin/my-cni-agent
```

## 4. Enable GCP IP forwarding on worker VMs

Every worker VM must have `canIpForward` enabled.

Check:

```sh
gcloud compute instances describe <node-name> \
  --zone <zone> \
  --format="get(canIpForward)"
```

Expected:

```text
True
```

If it is not enabled, export the instance config:

```sh
gcloud compute instances export <node-name> \
  --zone <zone> \
  --destination <node-name>.yaml
```

Edit the YAML and set:

```yaml
canIpForward: true
```

Apply:

```sh
gcloud compute instances update-from-file <node-name> \
  --zone <zone> \
  --source <node-name>.yaml \
  --most-disruptive-allowed-action REFRESH
```

Repeat for every worker node.

## 5. Create GCP VPC routes for pod CIDRs

Create one route per worker pod CIDR.

Example:

```text
c1-wn1 node pod CIDR: 10.10.1.0/24
c1-wn2 node pod CIDR: 10.10.2.0/24
VPC: cluster1-vpc
Zone: asia-south2-a
```

Routes:

```sh
gcloud compute routes create my-cni-c1-wn1-pods \
  --network cluster1-vpc \
  --destination-range 10.10.1.0/24 \
  --next-hop-instance c1-wn1 \
  --next-hop-instance-zone asia-south2-a \
  --priority 100
```

```sh
gcloud compute routes create my-cni-c1-wn2-pods \
  --network cluster1-vpc \
  --destination-range 10.10.2.0/24 \
  --next-hop-instance c1-wn2 \
  --next-hop-instance-zone asia-south2-a \
  --priority 100
```

Verify:

```sh
gcloud compute routes list --filter="name~my-cni"
```

## 6. Allow pod CIDR traffic in GCP firewall

Create an ingress firewall rule for pod-to-pod traffic.

Example for `10.10.0.0/16`:

```sh
gcloud compute firewall-rules create my-cni-allow-pods \
  --network cluster1-vpc \
  --direction INGRESS \
  --action ALLOW \
  --source-ranges 10.10.0.0/16 \
  --rules all
```

Verify:

```sh
gcloud compute firewall-rules list \
  --filter="network:cluster1-vpc AND name~my-cni"
```

## 7. Configure `my-cni-agent`

On every worker node, create:

```text
/etc/cni/net.d/my-cni-routes.json
```

Use the same `nodes` list on every worker, but set `nodeName` to the current
node.

For GCP, set `gateway` to the subnet gateway. In this cluster, the gateway was:

```text
10.0.0.1
```

Example for `c1-wn1`:

```json
{
  "nodeName": "c1-wn1",
  "gateway": "10.0.0.1",
  "nodes": [
    {
      "name": "c1-wn1",
      "podCIDR": "10.10.1.0/24",
      "internalIP": "10.0.0.3"
    },
    {
      "name": "c1-wn2",
      "podCIDR": "10.10.2.0/24",
      "internalIP": "10.0.0.2"
    }
  ]
}
```

Example for `c1-wn2`:

```json
{
  "nodeName": "c1-wn2",
  "gateway": "10.0.0.1",
  "nodes": [
    {
      "name": "c1-wn1",
      "podCIDR": "10.10.1.0/24",
      "internalIP": "10.0.0.3"
    },
    {
      "name": "c1-wn2",
      "podCIDR": "10.10.2.0/24",
      "internalIP": "10.0.0.2"
    }
  ]
}
```

Start the agent as root:

```sh
sudo my-cni-agent -config /etc/cni/net.d/my-cni-routes.json
```

The agent should install routes like:

```text
10.10.2.0/24 via 10.0.0.1 dev ens4
10.10.1.0/24 via 10.0.0.1 dev ens4
```

## 8. Enable Linux forwarding on every worker

Run on every worker node:

```sh
sudo sysctl -w net.ipv4.ip_forward=1
sudo sysctl -w net.ipv4.conf.all.forwarding=1
sudo sysctl -w net.ipv4.conf.default.rp_filter=0
sudo sysctl -w net.ipv4.conf.all.rp_filter=0
sudo sysctl -w net.ipv4.conf.ens4.rp_filter=0
sudo sysctl -w net.ipv4.conf.cni0.rp_filter=0
```

For a permanent setup, place these values in a file under `/etc/sysctl.d/`.

## 9. Validate cross-node pod traffic

Create one long-running pod on each node.

Example:

```sh
kubectl run test \
  --image=busybox:1.36 \
  --restart=Never \
  --overrides='{"spec":{"nodeName":"c1-wn1","containers":[{"name":"test","image":"busybox:1.36","command":["sleep","3600"]}]}}'
```

```sh
kubectl run test2 \
  --image=busybox:1.36 \
  --restart=Never \
  --overrides='{"spec":{"nodeName":"c1-wn2","containers":[{"name":"test2","image":"busybox:1.36","command":["sleep","3600"]}]}}'
```

Get pod IPs:

```sh
kubectl get pods -o wide
```

Ping across nodes:

```sh
kubectl exec -it test -- ping <test2-pod-ip>
kubectl exec -it test2 -- ping <test-pod-ip>
```

Expected:

```text
0% packet loss
```

## 10. Useful debugging commands

Check route lookup for forwarded pod traffic:

```sh
ip route get <remote-pod-ip> from <local-pod-ip> iif cni0
```

Watch traffic on source node:

```sh
sudo tcpdump -ni cni0 "icmp or arp"
sudo tcpdump -ni ens4 "icmp or arp"
```

Watch traffic on destination node:

```sh
sudo tcpdump -ni ens4 "icmp or arp"
sudo tcpdump -ni cni0 "icmp or arp"
```

Check local pod reachability from its host:

```sh
ping <local-pod-ip>
```

Check stale or wrong routes:

```sh
ip route | grep 10.10
```

Check failed neighbor resolution:

```sh
ip neigh show dev ens4
```

In GCP, peer node IPs might show `FAILED` if you try `onlink` routes. That is
why this CNI uses the GCP subnet gateway in `gateway` mode.
