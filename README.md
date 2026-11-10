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

VPN Gate 的接口一次只给一百来个节点（此刻实测 79 个），而且是按它自己算的分数取
头部，不是全量在线列表。换条路再拉一次，拿到的多半是另一批：实测主站和官方镜像
只有 8~10 个节点重合。池子小会连带影响「换一个节点」和「每个国家各开几个」——
同地区可换的候选很快就排干了。

fanout 拉两个源，按 hostname 去重合并：

- **官方主站**（CSV）：实时生成，失败时退到自建反代（见 `worker/`）
- **聚合中心**（自己的 [vpngate-agg](https://github.com/gebaolei/vpngate-agg) 服务，
  JSON 接口）：它把主站与 sites.aspx 上全部镜像站合并成 7 天累积池，自己再做
  TCP/UDP 可达与 OpenVPN 指纹探测，所以名单比官方那张大一个数量级。
  默认只收 `level=stable,reachable` 那两档

实测同一台机器上背靠背各跑一次：只拉主站 79 个节点；加上聚合中心 1244 个。

两个源的分工要说清楚，不然容易高估后者：

- 主站排在前，两边都有的节点以主站的分数/延迟与 IP 为准（主站实时生成，
  聚合中心的名单是它自己那台机器某一轮拉下的快照；家宽节点 IP 会变）
- 聚合中心跑在**另一台机器/另一条线路**上，它给的 `level`、延迟、纯净度说的都是
  那台机器的观测。只当「这个节点还活着」的预筛（死在哪都是死），不能当「从这台
  机器连得上」的依据。真能不能连，照样要过 fanout 自己的握手、出口 IP 校验、
  健康检查这三关
- 每 12 小时刷一轮，另外在启动后、以及自动重连且一整轮候选都挂了的时候刷新。
  启动只同步拉主站，聚合中心在后台补，免得把隧道恢复排在一次跨机器拉取后面
- 任何一个源失败都不影响其余的。官方源全挂而聚合中心有货时，用聚合中心的顶上；
  两边都挂才保留上一轮列表，不会清空。单个源返了一堆残缺节点也会被丢掉
  （上游给半截但交付完整时，解析是不会报错的）；聚合中心那条另有更硬的判据：
  它把自己吐了几行写在响应头里，对不上就按响应被截断处理
- 翻页是这个源的正常用法：每页最多 500 个节点，按 `X-Vpngate-Agg-Total` 翻到底。
  中途它自己刷新一轮的话窗口会飘几个，按 hostname 去重兜住

之前这里还接过一个第三方 GitHub 滚动节点池，已撤掉：那份池子只保证「六小时前
某个 Cloudflare 边缘 TCP 端口能连上」，又依赖别人的仓库哪天还在；自己搭聚合中心
之后它就没有存在价值了（保留的价值在于不依赖任何自建服务，需要的话翻 git 历史）。

聚合中心的地址与凭据（不设 `FANOUT_AGG_API` 就是不用这个源）：

```
FANOUT_AGG_API      https://你的agg/vpngate/api/iphone/
FANOUT_AGG_TOKEN    Bearer token。走请求头，不拼进 URL —— 源地址会进日志与界面
FANOUT_AGG_FILTER   附加筛选，默认 level=stable,reachable。也可以写
                    residential=1、country=JP、min_purity=70 之类
FANOUT_AGG_TIMEOUT  单页超时，默认 30s
FANOUT_AGG_MAX      这一轮最多收多少个，默认 1500
```

`level` 这一项值得自己调一下，因为它直接决定候选面。拿一个 1500 个节点的池子实测：

```
stable 565 + reachable 75 + down 137 + unknown 618
level=stable,reachable        →  640 个，22 个国家，US 10
level=stable,reachable,unknown → 1243 个，44 个国家，US 26
```

`unknown` 是“没探成功过、但也没连续两轮明确否定”的。收进来候选面大一倍、小国家
从 22 个涨到 44 个；代价是这些节点的可连性完全没验证过，而试一个连不上的节点
要 40 多秒。默认不收，想收就改 `FANOUT_AGG_FILTER`，不用改代码。

服务端侧要 `api.enabled=true` 配一个 token（≥16 字符），走公网务必套 HTTPS；
翻页、筛选、限流都是它现成的能力，fanout 不改它一行代码。
注意每个节点的 `.ovpn` 是内联在每一行里的（实测平均 9.8KB）：640 个节点正文
合计 6.4MB，1243 个约 12MB 常驻。嫌大就调小 `FANOUT_AGG_MAX` 或者收窄筛选。

其它环境变量（不设就用默认值）：

```
FANOUT_VPNGATE_TIMEOUT   单个 CSV 源的超时，默认 25s；带宽差的机器上 1.3 MB 可能要 60s
FANOUT_VPNGATE_MIRROR    主站挂了的兜底反代地址；设成空串就是只走直连
FANOUT_VPNGATE_MIRROR_KEY 那个反代的访问 key
FANOUT_OVPN_KEEPALIVE    openvpn 保活。默认 15/120，并且会拦掉 VPN Gate 下发的
                         ping / ping-restart（它推的是 3/10，家宽节点稍有拥塞就拆链路，
                         拆的十几秒里流量从母机出去）。设 off 则完全沿用上游值；
                         "10/60" 这种形式可以自己调
```

「新建出口」里能看到上次拉取时间、源成败（含聚合中心单独一条的数量），
也可以直接点「刷新节点列表」。

换机器部署前建议先在目标机器上跑一次真实拉取，看清楚拿回多少节点、覆盖多少国家：

```
# 只看聚合中心这一个源（要先 export FANOUT_AGG_API / FANOUT_AGG_TOKEN）
FANOUT_LIVE_TEST=1 go test -run TestLiveAggSource -v

# 完整刷新路径：选源 + 并发 + 合并 + 数量检查
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
