# ra-prefix-controller

A controller that keeps a Cilium `CiliumLoadBalancerIPPool` inside the IPv6 prefix advertised by RA. When the prefix changes, for example during ISP maintenance, the public LB IPs move to the new prefix. external-dns then updates DNS.

## Behavior
- Watches address changes on the configured NIC through netlink and derives the /64 from the SLAAC address with the longest preferred lifetime. A resync every 5 minutes repairs missed events and manual changes.
- Ignores deprecated addresses of the old prefix. If no prefix can be detected, the pool is left unchanged.
- Keeps the current prefix until it is deprecated or another prefix's preferred lifetime exceeds it by more than 30 minutes. This stops the pool from swapping between advertised prefixes, but if maintenance does not deprecate the old prefix, the switch can be delayed by up to about 35 minutes.
- Serves health checks on the node's loopback. `/healthz` reports that the sync loop is running, and `/readyz` reports that the latest sync succeeded. The pod becomes Not Ready while no prefix can be detected or during API errors, but it is not restarted.
- Creates the pool if it does not exist; otherwise updates only `spec.blocks`. Do not manage this pool with GitOps.

## Prerequisites
- The NIC receives RAs (`accept_ra=2` on nodes with `forwarding=1`).
- Cilium LB IPAM is enabled, and the target Services are advertised by NDP, ARP or similar.

## Installation
```
helm install ra-prefix-controller ./chart -n kube-system --set interface=enp3s0
```

| Value | Default | Description |
|---|---|---|
| `interface` | Required | NIC that receives RAs |
| `pool.name` | `ipv6-ndp-pool-gua` | Pool to manage |
| `pool.hostPart` | `::beef:0` | Host part placed after the prefix |
| `pool.prefixLength` | `112` | Prefix length of the pool |
| `pool.serviceSelector` | `io.cilium/lb-pool: gua` | Service selector set when the pool is created |
| `healthPort` | `18091` | Health check port (on the node's loopback) |

## Development
```
go test ./...
docker build -t ra-prefix-controller .
```
