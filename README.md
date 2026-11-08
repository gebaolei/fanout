# fanout

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 的公共节点变成本地 SOCKS5 端口：一个端口一个出口 IP。
再给每个出口挂一个节点链接，客户端连哪个端口就从哪个国家出去。

节点链接有三种管法：同机装了 3x-ui 或 xray-cf-lite 就接管它们的入站，
都没装则 fanout 自己跑 Xray，建站、改站、发链接都在同一个界面里完成。

![主界面](https://images.joeyblog.net/2026/7/27/fanout-dashboard.png)

四条隧道跑在一台机器上，四个端口对应四个国家的出口，母机自己的 IP 不受影响：

![出口验证](https://images.joeyblog.net/2026/7/26/fanout-6-exit-ip.png)

## 原理

每个节点跑在独立的 network namespace 里，netns 内启动官方 openvpn 客户端。
SOCKS5 监听在母机，出站连接用 `setns` 切进对应 netns 建立。

这样做的好处：VPN 的路由劫持只影响自己的 netns，不会切断母机的网络；
多个节点互不干扰，各自一个出口 IP。

```
客户端 ──> 母机 SOCKS5 :随机端口 ──> netns foN ──> openvpn ──> VPN Gate 节点
```

## 安装

需要 root，Linux（依赖 netns）。

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/fanout/main/install.sh)
```

会自动下载对应架构的预编译二进制。也可以 clone 仓库后在源码目录运行同一个脚本，
那样会从源码编译（需要 Go 1.21+）。

依赖（openvpn / curl / openssl / iproute / iptables）会按发行版自动装，
apt、dnf、yum、pacman、apk、zypper 都认。没装 3x-ui 时还会顺带下载一份
Xray 到 `/var/lib/fanout/bin/`，装了则跳过，入站交给面板管。

服务用 systemd 或 OpenRC 都能装，装完自动开机自启。

**Alpine** 默认不带 bash，先装一下：

```bash
apk add bash curl
bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/fanout/main/install.sh)
```

另外 fanout 要在 netns 里跑 openvpn，**宿主必须放开 `/dev/net/tun`**。
不少 LXC 小鸡没给这个权限，`ls /dev/net/tun` 不存在且 `mknod` 报
Operation not permitted 的话，这台机器用不了，跟发行版无关。

装完敲 `f` 打开管理菜单：

![管理菜单](https://images.joeyblog.net/2026/7/26/fanout-7-menu.png)

装完会打印管理界面地址、访问路径和口令：

```
管理界面  http://<你的IP>:8899/gwPuWHvaNr/
访问口令  f81120ac328d11c11b
```

路径和口令都是随机生成的，分别存在 `/var/lib/fanout/basepath` 和
`/var/lib/fanout/password`。路径不对一律返回 404，扫端口的看不到这里跑着什么。

## 使用

界面以**出口**为单位：一行就是一条隧道加上挂在它上面的节点链接。

点「新建出口」，选地区和数量，再选一个已有节点作模板，提交后 fanout 会并行
拉起隧道、为每个出口复制一份节点链接并绑好，进度按目标逐条回报。原来要手点
五步跨两栏的事，现在一次点击十几秒完成。

地区里还有个「每个国家」：每个有空闲节点的国家各开几条，一次把所有地区铺开。
槽位不够时先开节点多的地区，不会中途报错。

![新建出口](https://images.joeyblog.net/2026/7/27/fanout-wizard.png)

每行右侧两个按钮：换一个节点（出口 IP 变、端口不变，已分发的客户端配置不用改），
或者停掉这个出口。换节点会避开这条出口之前用过的，连点几次每次都是新 IP。

出口和节点的名字自动起成 `🇯🇵 日本 243`（国旗 + 国家 + 出口 IP 末段）。
自己改过的名字不会被覆盖——换节点时只重写 fanout 自己起的那些。

点节点名进详情，可以改端口、备注、启停，管理客户端，以及改绑到别的出口：

![节点详情](https://images.joeyblog.net/2026/7/27/fanout-detail.png)

一个入站可以挂多套客户端凭据，分发给不同的人；每套都能单独重置，
重置后旧链接立即失效。

「导出链接」一次性拿到所有节点链接：

![导出链接](https://images.joeyblog.net/2026/7/27/fanout-export.png)

### 订阅

节点多了就别一条条复制了。点「订阅」拿一条地址，填进客户端的订阅里，
以后加出口、删出口都会自己跟上。换节点不影响它——端口不变，链接也不变。

地址形如 `http://<服务器IP>:<端口>/<访问路径>/sub?token=<口令>`。
默认出 base64，各家客户端通吃；想看明文在后面加 `&target=links`。
默认只放绑了出口的节点，走直连的不往里混；想全都要就加 `&bound=0`。

订阅要给客户端直接拉，所以这条地址不要登录，**那串 token 就等于密码**，别发群里。
泄露了点「换一串口令」，旧地址立刻失效。

### 只用家宽

VPN Gate 的清单里混着一批它自己的机房服务器（`public-vpn-*` 和
`219.100.37.0/24`），出口一眼看得出是数据中心，还更容易满员。
设置里「只用家宽节点」默认开着，挑节点、地区可用数、自动重连的备选都只看
志愿者家宽。想连机房的一起用就把它关掉。

开关只管新挑的节点，已经跑着的出口不会因为改设置被换掉。

### 节点列表从哪来

VPN Gate 的接口一次只给一百来个节点，而且是按它自己算的分数取头部，不是全量在线
列表。换条路再拉一次，拿到的多半是另一批：实测主站和官方镜像只有 8~10 个节点重合。
池子小会连带影响「换一个节点」和「每个国家各开几个」——同地区可换的候选很快就排干了。

所以 fanout 会同时拉几个源再按 hostname 去重：

- 主站排在前，两边都有的节点以主站的分数/延迟为准（主站实时生成，镜像是缓存快照）
- 官方镜像先按网段和地区去重再去拉：列表页上五个克罗地亚镜像其实在同一处机房，
  其中两两的节点列表逐字节相同
- 每轮最多两个镜像，然跨轮次轮换。实测主站 + 两个不同地点的镜像约 277 个节点，
  已经拿到 95% 的收益，再加同机房的镜像是 0 增益（不同机器上会有出入）
- 每 12 小时刷一轮，另外在启动后、以及自动重连且一整轮候选都挂了的时候刷新。
  启动只同步拉主站，镜像在后台补，免得把隧道恢复排在多源拉取后面
- 任何一个源失败都不影响其余的；全部失败时保留上一轮列表，不会清空。
  单个源返了一堆残缺节点也会被丢掉（上游给半截但交付完整时，解析是不会报错的）
- 列表页的解析结果存 `<workDir>/mirrors.json`，重启先读盘，不等网络

「新建出口」里能看到上次拉取时间与各源成败，也可以直接点「刷新节点列表」。

#### 第三方滚动节点池

上面三个源拿的都是源站那份“按分数取头部”的 ~100 个。还有人把 VPN Gate 的清单
做成了一个 7 天滚动的池子（GitHub 仓库，jsDelivr / GitHub Raw 分发），并且自己
养了两个 Cloudflare Worker 做 TCP 探测。fanout 把它当第 4 个源，只收“探测
6 小时内可达”的那批：

- 实测（同一台机器上背靠背各跑一次）：合并后节点 281 → 973，家宽覆盖 14 → 28 个国家。
  选面大得多，“换一个节点”不会翻来覆去就那几个 IP
- 不会被 VPN Gate 限流：只是从 CDN 读一份 30 分钟一轮的静态副本，
  不吃源站配额（主站被限流时它还能顶上）
- 每个节点的 .ovpn 按 sha256 分文件存，只有真要连那个节点时才去取（13KB 一个）。
  列表里这几百个节点不占内存，也不会提前下载
- 探测只到 TCP 端口层面，而且是从 Cloudflare 边缘探的 —— 不代表从这台机器连得上，
  所以这些节点照样要过 fanout 自己的握手、出口 IP 校验和健康检查。
  实测从部署机（到美国约 20ms、到日本约 134ms）抽测：US 8/10、JP 9/10、
  KR 10/10 确实 TCP 可达，可达率不低，但毕竟不是 OpenVPN 握手测试
- 自动重连时，源站列表里的实时节点优先，池子里的节点补位（重连要的是
  “能连回来”，不是“分数高”）；新开出口和手动换节点则不区分
- 取数按上游规定的姿势：先读 1KB 的索引，固定 data_commit，再按字节数与 sha256
  校验正文；commit 没变就不重拉那 7.1MB。过滤后的列表存 `<workDir>/pool.json`，
  重启先读盘
- 不要求节点此刻还在源站 CSV 里（候选增量就在这里），但同名节点以 CSV 的实时
  分数/延迟为准。实测两边的 IP 会不一致（家宽 IP 会变），这时候用 CSV 的

环境变量（不设就用默认值）：

```
FANOUT_VPNGATE_SITES     列表页地址。默认官方的 /en/sites.aspx；设成空串就是不用镜像
FANOUT_VPNGATE_SOURCES   每轮取几个镜像，默认 2；设 0 就是只用主站（即旧行为）
FANOUT_VPNGATE_TIMEOUT   单个源的超时，默认 25s；带宽差的机器上 1.3 MB 可能要 60s
FANOUT_VPNGATE_POOL      第三方节点池的索引地址；设成空串就是不用这个源
FANOUT_VPNGATE_POOL_TIMEOUT 拉池文件的超时，默认 60s（那份文件 7.1MB）
FANOUT_VPNGATE_POOL_CDN  池文件的 CDN 地址（自建镜像、国内加速时改）
FANOUT_VPNGATE_POOL_RAW  池文件的 GitHub Raw 地址（CDN 挂了的兜底）
FANOUT_OVPN_KEEPALIVE    openvpn 保活。默认 15/120，并且会拦掉 VPN Gate 下发的
                         ping / ping-restart（它推的是 3/10，家宽节点稍有拥塞就拆链路，
                         拆的十几秒里流量从母机出去）。设 off 则完全沿用上游值；
                         "10/60" 这种形式可以自己调
```

换机器部署前建议先在目标服务器上跑一次探测，看清楚哪些源通、重合度多少：

```
FANOUT_LIVE_TEST=1 FANOUT_LIVE_TIMEOUT=90s go test -run TestLiveMirrorProbe -v
```

想看多源聚合（含第三方节点池）在这台机器上到底拿回多少节点，跑这个；
前面加 `FANOUT_VPNGATE_POOL=` 就是关掉第三方池作对比：

```
FANOUT_LIVE_TEST=1 FANOUT_VPNGATE_TIMEOUT=60s go test -run TestLiveRefreshNodes -v
```

### 节点链接从哪来

同机装了 3x-ui 就直接接管面板里的入站，面板端口、路径、API token 全自动探测，
开了 SSL 也能识别。没装 3x-ui 时 fanout 自己跑一个 Xray，界面上多一个「新建节点」
按钮，可以选协议（VLESS / VMess / Trojan）、传输（TCP / WebSocket / gRPC /
HTTPUpgrade / XHTTP）和安全层（无 / TLS / REALITY）。

![新建节点](https://images.joeyblog.net/2026/7/27/fanout-newnode.png)

REALITY 的密钥对和 shortId 自动生成；TLS 不填证书就生成自签的，分享链接会带上
证书指纹让客户端固定信任。也可以填自己的证书路径。

接管 3x-ui 和自建这两种模式下，改端口、启停、加删客户端、绑定出口的操作完全一致，
用起来没有区别。

装了 [xray-cf-lite](https://github.com/byJoey/xray-cf-lite) 的机器会自动接管它生成的
三个节点。这个模式下节点归 xray-cf-lite 管，fanout 只负责给每个节点指定走哪条出口，
所以界面上不提供新建、删除和改节点的入口——想改端口或 UUID 去 xray-cf-lite 那边改。
两边共用同一份 Xray 配置，fanout 只往里加自己前缀的出站和分流规则，互不覆盖。

后端在设置面板里可以随时切换，本机没装的会置灰并说明原因；也可以用
`-panel 3x-ui` / `-panel native` / `-panel xray-cf-lite` 启动参数固定。
界面里选过之后会记住，重启仍然生效。

## 运维

装完后敲 `f` 打开管理菜单：启停、看日志、查隧道、改端口/口令/访问路径、更新、卸载。

```
  状态      运行中
  版本      fanout v0.1.1
  开机自启  enabled

  管理地址  http://1.2.3.4:8899/gwPuWHvaNr/
  访问口令  f81120ac328d11c11b

   1) 启动          2) 停止
   3) 重启          4) 查看日志
   5) 隧道列表      6) 连接信息
   7) 改端口        8) 改口令
   9) 改访问路径   10) 开机自启开关
  11) 更新         12) 卸载
```

也可以直接带参数用：

```bash
f info       # 连接信息
f list       # 隧道列表
f restart    # 重启
f log        # 跟踪日志
f update     # 更新到最新版
f uninstall  # 卸载
```

隧道状态存在 `/var/lib/fanout/state.json`，重启后自动恢复，端口保持不变。

健康检查每 10 秒跑一次，比对出口 IP 是否还是建立隧道时那个——openvpn 挂掉后
netns 仍能经母机 NAT 出网，只看通不通会漏判。真不通时不会当场下结论：先隔几秒
复查几轮，给 openvpn 的自愈留出窗口（它重启链路实测 3 秒就恢复），自己能回来就
不拆隧道。出口 IP 变成本机公网地址则不等，直接拆——那种情况每一秒都在泄露
服务器真实地址。

确认掉线后重连时，刚连不上的节点在 30 分钟内不再被排到候选首位，否则会出现
"拆掉 → 又连回同一个连不上的节点 → 再拆"的循环。这个短期黑名单与界面上的
「换节点」历史是分开的两套：那边是你明确不要的出口 IP，会一直避开。

自建 Xray（native 后端）的默认出站是 block 而不是直连：绑着的出口正在重连时，
那个入站的流量会被吃掉，而不是从服务器 IP 直接出去——宁断不漏，重连通常十几秒
就好。主动选了「直连」的入站不受影响，它有自己指向直连的规则。

## 已知限制

- SOCKS5 支持 CONNECT 和 UDP ASSOCIATE，DNS/QUIC 这类 UDP 也走隧道
  （感谢 [@zsawi](https://github.com/zsawi) 的 [#22](https://github.com/byJoey/fanout/pull/22)）。
  域名仍在本机解析。
- VPN Gate 是志愿者节点，有相当比例已下线或满员（`AUTH_FAILED`）。
  启动时连不上会自动顺着同地区候选往下试，最多 6 个。
- 管理界面只有随机路径 + 口令登录，没有 HTTPS。放公网建议前面套一层反代。
  订阅地址同理，token 是明文走的。
- 换节点会避开这条出口之前用过的节点（最多记 16 个），同地区都换过一轮后
  从头开始。一个地区只有一个可用节点时换不动，界面会直接说明原因。

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），
本工具只是调用其公开的节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。
使用时请遵守 VPN Gate 的条款和你所在地的法律。

## 交流

- 交流群：<https://t.me/+ft-zI76oovgwNmRh>
- 视频教程：<https://youtube.com/@joeyblog>
- 博客：<https://joeyblog.net>

用着有问题、或者想要什么功能，去群里说或提 issue。
