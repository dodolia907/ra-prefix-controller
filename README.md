# ra-prefix-controller

RAで配布されるIPv6 prefixに、Ciliumの `CiliumLoadBalancerIPPool` を追従させるコントローラ。ISPの工事などでprefixが変わっても、公開用LB IPが新しいprefixへ移る。DNSはexternal-dnsが追従する。

## 動作
- netlinkで指定NICのアドレス変化を受け取り、SLAACアドレスのうちpreferred lifetimeが最長のものから/64を求める。5分ごとの再同期は、取りこぼしと手動変更の修復用。
- 旧prefixのdeprecatedアドレスは無視する。prefixを検出できないときはプールを変更しない。
- プールがなければ作成し、あれば `spec.blocks` だけを更新する。このプールはGitOps管理に含めないこと。

## 前提
- NICがRAを受けていること（`forwarding=1` のノードでは `accept_ra=2`）。
- Cilium LB IPAMが有効で、対象ServiceがNDP/ARPなどで広報されること。

## インストール
```
helm install ra-prefix-controller ./chart -n kube-system --set interface=enp3s0
```

| value | 既定値 | 説明 |
|---|---|---|
| `interface` | 必須 | RAを受けるNIC |
| `pool.name` | `ipv6-ndp-pool-gua` | 管理するプール |
| `pool.hostPart` | `::beef:0` | prefixの後ろに置くホスト部 |
| `pool.prefixLength` | `112` | プールのprefix長 |
| `pool.serviceSelector` | `io.cilium/lb-pool: gua` | プール作成時のServiceセレクタ |

## 開発
```
go test ./...
docker build -t ra-prefix-controller .
```
