package main

import (
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// vpngateAPI 是节点列表接口。写成 var 是为了测试能把主站指向 httptest，
// 跟 fetchNodesWith 把直连地址拆成参数是同一个思路。
var vpngateAPI = "https://www.vpngate.net/api/iphone/"

// vpngateMirror 是直连拿不到节点列表时的兜底（Cloudflare Worker 反代）。
// 用 FANOUT_VPNGATE_MIRROR 可以换成自己的地址，设成空字符串就只走直连。
const vpngateMirror = "https://p.xy.kg/vpngate"

// mirrorKey 只是让反代不被爬虫和端口扫描白嫖，不是安全边界。
const mirrorKey = "8rhIFzFKRJMFAe-xP5OQPclDEvSjKlHo"

func mirrorURL() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_MIRROR"); ok {
		return strings.TrimSpace(v)
	}
	return vpngateMirror
}

func mirrorAccessKey() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_MIRROR_KEY"); ok {
		return strings.TrimSpace(v)
	}
	return mirrorKey
}

// csvMaxBytes 是单个节点列表响应的体积上限。官方 CSV 实测 ~1.33 MB，
// 超出这个数就是响应被截断（反代吞半截）或者被换成了其什么大的东西，
// 两种都不能当正常名单用。聚合中心那条源自己有上限与行数校验（见 aggsource.go）。
const csvMaxBytes = 4 << 20

// nodeTransport 是拉官方节点列表与其反代兜底共用的传输层。
//
// DisableCompression 不是优化，是必需的：官方源对带 `Accept-Encoding: gzip`
// 的请求会先回 200 再一个字节都不发，一直挂到客户端超时（同一个 URL 用 curl
// 不带这个头，1.3 MB 几秒拉完）。Go 的默认 Transport 会自动加这个头，
// 不关掉就等于这条源永远失败。CSV 本来就是纯文本，压不压差不了多少。
//
// 自家聚合中心不用这个 transport：那是自己的服务，gzip 能省一半以上流量。
var nodeTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}()

// Node 是一个 VPN Gate 节点。
type Node struct {
	HostName    string  `json:"hostname"`
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Ping        int     `json:"ping"`
	SpeedMbps   float64 `json:"speed_mbps"`
	Sessions    int     `json:"sessions"`
	Config      string  `json:"-"` // 解码后的 .ovpn 内容
	// Residential 表示这是志愿者的家庭宽带，不是 VPN Gate 自己的机房服务器。
	Residential bool `json:"residential"`
}

// vpngate 自营服务器所在的网段。这些是学术网络里的机房机器，
// 不是志愿者家宽：出口 IP 一眼能看出来是数据中心，而且更容易满员。
var vpngateOwnNets = []string{"219.100.37.0/24"}

// isResidential 判断一个节点是不是志愿者家宽。
//
// 两条判据都是实测出来的：
//   - hostname 以 public-vpn- 开头的是 vpngate 自营服务器
//   - 219.100.37.0/24 段同样是自营（学术网络机房）
//
// 剩下的是志愿者用自家宽带跑的，出口 IP 属于普通运营商。
func isResidential(hostName, ip string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(hostName)), "public-vpn-") {
		return false
	}
	addr := net.ParseIP(strings.TrimSpace(ip))
	if addr == nil {
		// IP 解析不出来时不敢断言，按家宽放过，让连通性测试去淘汰
		return true
	}
	for _, cidr := range vpngateOwnNets {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil && block.Contains(addr) {
			return false
		}
	}
	return true
}

// fetchNodes 拉取并解析 VPN Gate 的节点列表（单源）。
// 先直连；连不上或者拿回来的内容不对（被拦截、返回门户页）就换反代再试一次。
// 返回的列表已按速度降序排列。
//
// 启动时走这条路（同步、只拉主站，不拖慢隧道恢复）；
// 运行期的多源聚合走 fetchAggregated。
func fetchNodes(timeout time.Duration) ([]Node, error) {
	return fetchNodesWith(vpngateAPI, timeout)
}

// nodeFetchTimeout 是单个源的超时，不是整轮的超时。
// 多个源并发，所以整轮耗时基上就等于这个值。
// 注意 http.Client.Timeout 覆盖的是整个请求（含读 body）：
// 实测 46 KB/s 的链路上 1.3 MB 要 30 秒，带宽差的机器要调大。
func nodeFetchTimeout() time.Duration {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_TIMEOUT"); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	}
	return 25 * time.Second
}

// fetchNodesWith 把直连地址拆成参数，方便测试两条分支。
func fetchNodesWith(direct string, timeout time.Duration) ([]Node, error) {
	nodes, err := fetchNodesFrom(direct, "", timeout)
	if err == nil {
		return nodes, nil
	}
	mirror := mirrorURL()
	if mirror == "" {
		return nil, err
	}
	nodes, mirrorErr := fetchNodesFrom(mirror, mirrorAccessKey(), timeout)
	if mirrorErr != nil {
		return nil, fmt.Errorf("直连失败(%v)；反代也失败: %w", err, mirrorErr)
	}
	return nodes, nil
}

func fetchNodesFrom(url, key string, timeout time.Duration) ([]Node, error) {
	client := &http.Client{Timeout: timeout, Transport: nodeTransport}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("拉取节点列表失败: %w", err)
	}
	if key != "" {
		req.Header.Set("X-Fanout-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取节点列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取节点列表失败: HTTP %d", resp.StatusCode)
	}
	// 多读 1 字节用来判断超限：直接 LimitReader 到上限会把半截列表
	// 当成完整列表写进 m.nodes（parseNodeCSV 对结束标记 `*` 是跳过、不校验）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, csvMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取节点列表失败: %w", err)
	}
	if len(raw) > csvMaxBytes {
		return nil, fmt.Errorf("节点列表超过 %d 字节，按异常处理", csvMaxBytes)
	}
	return parseNodeCSV(string(raw))
}

// sourceStat 是一个源的拉取结果，日志与界面诊断用。
type sourceStat struct {
	URL   string `json:"url"`
	Nodes int    `json:"nodes"`
	MS    int64  `json:"ms"`
	Err   string `json:"err,omitempty"`
}

// 并发 3：现在官方 CSV 源只有主站那一个（失败退自建反代），并发数是为
// 以后再加源留的。真并发起来最坏就是一个单源超时，不会变成两个波次。
const maxSourceWorkers = 3

// csvHeadLine 返第一个非空行（去 BOM）以及它是不是 VPN Gate 的开头标记。
// 后面那截只用于报错预览，压成一行并截短，避免把整页 HTML 灌进日志。
func csvHeadLine(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, "\ufeff"), "\ufeff"))
		if line == "" {
			continue
		}
		preview := strings.Join(strings.Fields(line), " ")
		r := []rune(preview)
		if len(r) > 60 {
			preview = string(r[:60]) + "…"
		}
		return preview, strings.EqualFold(line, "*vpn_servers")
	}
	return "", false
}

// 单个源至少要有多少个节点才算正常。实测单站是 97~101。
const minNodesPerSource = 20

// fetchAggregated 并发拉多个源，按 hostname 去重合并。
//
// sources[0] 视为主站：它失败时会像以前一样退到自建反代。
// prevByURL 是上一轮每个源各自返回的节点数，用于“骤减”判断；
// 首轮或新出现的源传空 map。不能用合并后的总数当基准：
// 单源只返 ~100 个，合并后是 277，拿 277 去卡单源会把所有源全判成异常。
// 只要有一个源成功就返回结果；全部失败才报错，调用方保留旧列表。
func fetchAggregated(sources []string, timeout time.Duration, prevByURL map[string]int) ([]Node, []sourceStat, error) {
	if len(sources) == 0 {
		return nil, nil, fmt.Errorf("没有可用的节点源")
	}
	per := make([][]Node, len(sources))
	stats := make([]sourceStat, len(sources))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxSourceWorkers)
	for i, u := range sources {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			stats[i].URL = u
			t0 := time.Now()
			nodes, err := fetchOneSource(i, u, timeout, prevByURL[u])
			stats[i].MS = time.Since(t0).Milliseconds()
			if err != nil {
				stats[i].Err = err.Error()
				return
			}
			stats[i].Nodes = len(nodes)
			per[i] = nodes
		}(i, u)
	}
	wg.Wait()
	merged := mergeNodeSources(per)
	if len(merged) == 0 {
		return nil, stats, fmt.Errorf("所有节点源都失败")
	}
	return merged, stats, nil
}

// fetchOneSource 拉一个源。i==0 是主站，失败时要退到自建反代（既有语义）。
func fetchOneSource(i int, url string, timeout time.Duration, prevCount int) ([]Node, error) {
	var nodes []Node
	var err error
	if i == 0 {
		nodes, err = fetchNodesWith(url, timeout)
	} else {
		nodes, err = fetchNodesFrom(url, "", timeout)
	}
	if err != nil {
		return nil, err
	}
	if err := nodeCountPlausible(len(nodes), prevCount); err != nil {
		return nil, err
	}
	return nodes, nil
}

// nodeCountPlausible 是数量合理性检查，不能省。
//
// 传输层中断时 io.ReadAll 会报 unexpected EOF；但上游或中间人给出一个
// “更短但完整”的响应时，parseNodeCSV 完全成功、不报错，残缺列表会直接
// 写进 m.nodes，表现为池子凭空小一半。只能靠数量兜底。
//
// prev 是同一个源上一轮的数量（不是合并后的总数）：每站规模各自稳,
// 跨站比没意义。
func nodeCountPlausible(n, prev int) error {
	if n < minNodesPerSource {
		return fmt.Errorf("节点数过少(%d)，按异常处理", n)
	}
	if prev > 0 && n*2 < prev {
		return fmt.Errorf("节点数比上一轮(%d)少一半以上，按异常处理", prev)
	}
	return nil
}

// mergeNodeSources 按 hostname 去重合并，靠前的源优先。
//
// 主站是实时生成的，聚合中心的名单是它自己那台机器某一刻的快照：
// Score/Ping/Speed 与主站不一致且是旧值，所以两边都有时要用主站的。
// 实测 7 个源里 hostname 与 IP 一一对应（多 IP 数 = 0），所以拿 hostname
// 当键是安全的；真撞上不一致就沿用优先级高的那条，不去猜。
func mergeNodeSources(perSource [][]Node) []Node {
	at := map[string]int{}
	var out []Node
	for _, list := range perSource {
		for _, n := range list {
			if i, ok := at[n.HostName]; ok {
				if out[i].IP != n.IP {
					log.Printf("节点 %s 在不同源里 IP 不一致（%s / %s），沿用 %s",
						n.HostName, out[i].IP, n.IP, out[i].IP)
				}
				continue
			}
			at[n.HostName] = len(out)
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SpeedMbps > out[j].SpeedMbps })
	return out
}

// parseNodeCSV 解析 VPN Gate 的 CSV。首行是 "*vpn_servers"，
// 第二行是以 '#' 开头的表头，末行是 "*"。
func parseNodeCSV(body string) ([]Node, error) {
	// 先确认首行。限流或中间拦截页也是 HTTP 200，但返的是 HTML，
	// 直接丢进 csv 只会得到「bare " in non-quoted-field」这种看不出
	// 所以然的报错（<!DOCTYPE html><html lang="..." 的引号正好落在 22~23 列）。
	if head, ok := csvHeadLine(body); !ok {
		return nil, fmt.Errorf("返回的不是节点列表（首行 %q，疑似限流或拦截页）", head)
	}
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		kept = append(kept, strings.TrimPrefix(line, "#"))
	}
	if len(kept) < 2 {
		return nil, fmt.Errorf("节点列表格式异常: 有效行不足")
	}

	r := csv.NewReader(strings.NewReader(strings.Join(kept, "\n")))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析节点 CSV 失败: %w", err)
	}

	header := records[0]
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	need := []string{"HostName", "IP", "CountryLong", "CountryShort", "Ping", "Speed", "OpenVPN_ConfigData_Base64"}
	for _, k := range need {
		if _, ok := idx[k]; !ok {
			return nil, fmt.Errorf("节点列表缺少字段 %s", k)
		}
	}

	var nodes []Node
	for _, rec := range records[1:] {
		get := func(k string) string {
			i := idx[k]
			if i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		cfgB64 := get("OpenVPN_ConfigData_Base64")
		if cfgB64 == "" || get("HostName") == "" {
			continue
		}
		cfg, err := base64.StdEncoding.DecodeString(cfgB64)
		if err != nil {
			continue
		}
		ping, _ := strconv.Atoi(get("Ping"))
		speed, _ := strconv.ParseFloat(get("Speed"), 64)
		sessions, _ := strconv.Atoi(get("NumVpnSessions"))
		nodes = append(nodes, Node{
			HostName:    get("HostName"),
			IP:          get("IP"),
			Residential: isResidential(get("HostName"), get("IP")),
			Country:     get("CountryLong"),
			CountryCode: get("CountryShort"),
			Ping:        ping,
			SpeedMbps:   speed / 1e6,
			Sessions:    sessions,
			Config:      string(cfg),
		})
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("节点列表为空")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].SpeedMbps > nodes[j].SpeedMbps })
	return nodes, nil
}
