package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// vpngateSitesPage 是 VPN Gate 的镜像站列表页。
// 页面上的镜像会变（页面自己标着"更新于 …"），所以这份列表要缓存，
// 而且解析失败时必须能退回上一次的结果，不能让主流程跟着挂。
const vpngateSitesPage = "https://www.vpngate.net/en/sites.aspx"

const (
	// 列表页本身只有十几 KB。给 1 MB 已经极其宽松，超过就是页面不对。
	sitesPageMaxBytes = 1 << 20
	// 节点 CSV 实测 ~1.33 MB。上限用于 P2 的多源拉取，这里先定义好，
	// 免得两处各自取一个数。
	csvMaxBytes = 4 << 20
	// 页面给多少就拉多少是不行的。当前页面上只有 6 个镜像，
	// 8 已经留了余量；再多说明页面被换过，或者有人在往里塞东西。
	maxMirrorEntries = 8
	// 每一轮取几个镜像。实测主站 + 2 个不同地点的镜像 = 277/291，
	// 已经拿到 95% 的收益；第 4 个只多 9 个节点，第 6、7 个多 0 个。
	maxMirrorsPerFetch = 2
	// 镜像列表页变动很慢（页面上写着每天更新时间），12 小时一轮。
	mirrorListMaxAge = 12 * time.Hour
)

// nodeTransport 是拉节点列表与镜像列表共用的传输层。
//
// DisableCompression 不是优化，是必需的：官方镜像站对带 `Accept-Encoding: gzip`
// 的请求会先回 200 再一个字节都不发，一直挂到客户端超时（同一个 URL 用 curl
// 不带这个头，1.3 MB 几秒拉完）。Go 的默认 Transport 会自动加这个头，
// 不关掉就等于所有镜像源永远失败。CSV 本来就是纯文本，压不压差不了多少。
var nodeTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}()

// sitesPageURL 可以指到 /cn/ 版本或者自建副本；设成空字符串就彻底不拉镜像列表。
func sitesPageURL() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_SITES"); ok {
		return strings.TrimSpace(v)
	}
	return vpngateSitesPage
}

// mirrorEntry 是列表页上的一个镜像。Base 不带语言后缀（/en/、/cn/）。
type mirrorEntry struct {
	Base     string `json:"base"`
	Location string `json:"location,omitempty"`
}

// blockedNets 是 net.IP.IsPrivate 覆盖不到的保留段。两段必须点名：
//
//	100.64.0.0/10  运营商级 NAT，Tailscale/ZeroTier 之类会用到
//	198.18.0.0/15  基准测试段，也是 fake-IP / TUN 代理常用的劫持段。
//	               本机跑 TUN 时 www.vpngate.net 会被解析到这里，
//	               列表里真出现这种地址就说明拿回来的内容不可信
//
// 其余（127/8、10/8、172.16/12、192.168/16、169.254/16、组播）由下面的
// IsXxx 判断覆盖，这里只补文档化的杂项段。
var blockedNets = []string{
	"0.0.0.0/8",
	"100.64.0.0/10",
	"198.18.0.0/15",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
}

var blockedPrefixes = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range blockedNets {
		if _, block, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, block)
		}
	}
	return out
}()

// blockedAddrReason 返回拒绝这个地址的原因，空串表示放行。
//
// fanout 以 root 跑，而列表页返回的是任意的 IP:PORT —— 不校验等于
// 把一个是 SSRF 入口直接交给第三方页面。本机还有 openvpn、netns、
// 管理界面和已分发的 SOCKS5 端口，被一个假列表指过去都是事。
func blockedAddrReason(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "未指定地址"
	case ip.IsLoopback():
		return "回环地址"
	case ip.IsPrivate():
		return "私有网段"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地地址"
	case ip.IsMulticast():
		return "组播地址"
	}
	for _, block := range blockedPrefixes {
		if block.Contains(ip) {
			return "保留段 " + block.String()
		}
	}
	return ""
}

// validateMirrorBase 校验列表页给出的一个地址，返回归一化后的站点根
// （scheme://host:port，不带 /en/ 这种后缀）。
//
// localPorts 用来拒绝"镜像端口正好是这台机器自己在监听的端口"，
// 传 nil 表示不做这项检查（解析阶段用 nil，真正拉取前必须传）。
func validateMirrorBase(raw string, localPorts func(int) bool) (string, net.IP, int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil, 0, fmt.Errorf("镜像地址无法解析: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", nil, 0, fmt.Errorf("镜像协议不被允许: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", nil, 0, fmt.Errorf("镜像地址缺少主机")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		// 只收 IPv4 字面量。域名要先解析再校验，而且解析结果能随 DNS 变，
		// 校验和连接之间还能重新绑定（DNS rebinding）。页面上给的全是 IP，
		// 所以先把域名拒掉；真要支持域名镜像时再补"解析后二次校验"。
		return "", nil, 0, fmt.Errorf("镜像必须是 IPv4 字面量，不接受域名: %q", host)
	}
	if reason := blockedAddrReason(ip); reason != "" {
		return "", nil, 0, fmt.Errorf("镜像地址 %s 不被允许: %s", ip, reason)
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", nil, 0, fmt.Errorf("镜像端口不合法: %q", p)
		}
		port = n
	} else {
		// 页面上每个镜像都带端口。不带端口时按协议默认放行，
		// 免得上游哪天改用 80/443 就直接解析不出东西。
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	if localPorts != nil && localPorts(port) {
		return "", nil, 0, fmt.Errorf("镜像端口 %d 与本机正在监听的端口冲突", port)
	}
	base := scheme + "://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
	return base, ip, port, nil
}

var (
	// 页面把镜像列表放在 <span id="Label1"> 里，里面紧跟一个 <ul>，形如：
	//   <li><strong><span ...><a href='http://150.40.105.16:33958/en/'>...</a></span>
	//       </strong>&nbsp;(Mirror location: Croatia (LOCAL Name: Hrvatska))</li>
	// 收口必须用 </ul>：每个 <li> 内部都有 </span>，按 </span> 收会直接切在第
	// 一个镜像中间，什么都解析不出来（踩过）。
	reLabel1   = regexp.MustCompile(`(?is)<span[^>]+id=['"]Label1['"][^>]*>(.*?)</ul>`)
	reListItem = regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	reHref     = regexp.MustCompile(`(?is)href=['"]([^'"]+)['"]`)
	reLocation = regexp.MustCompile(`(?i)Mirror location:\s*([^<]*)`)
)

// cleanMirrorLocation 把 "Croatia (LOCAL Name: Hrvatska)" 收成 "Croatia"。
// 页面上的文案格式不统一（有的带本地名，有的只有国家名），取第一段最稳。
func cleanMirrorLocation(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexAny(s, "(（"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimRight(s, ")）,;")
	return strings.TrimSpace(s)
}

func locationKey(location string) string {
	return strings.ToLower(cleanMirrorLocation(location))
}

// parseMirrorPage 从列表页取镜像条目。
//
// 先只认 Label1 那块（页面上镜像列表就在里面）。那块解析不出东西
// 就退回全页扫描 —— 上游改模板时至少还能拿到源，代价是没有 location，
// 选源时只能靠网段去重。
// 全页扫描不会误收导航链接：校验要求 IPv4 字面量 + 端口 + http(s)，
// 页面上那些 softether.org、/en/about.aspx 之类的链接一律过不了。
func parseMirrorPage(body string) ([]mirrorEntry, error) {
	var out []mirrorEntry
	if m := reLabel1.FindStringSubmatch(body); len(m) == 2 {
		out = scanMirrorItems(m[1])
	}
	if len(out) == 0 {
		// 到这一步连 <li> 都没有了（模板大改），只能扫一切链接。
		// 这种路径拿不到 location，所以选源时只剩网段去重可用。
		out = scanMirrorLinks(body)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("镜像列表页里没有解析出任何可用镜像")
	}
	if len(out) > maxMirrorEntries {
		log.Printf("镜像列表页给出了 %d 个镜像，只取前 %d 个", len(out), maxMirrorEntries)
		out = out[:maxMirrorEntries]
	}
	return out, nil
}

// scanMirrorItems 扫一块 HTML 里的 <li>，把过得了校验的镜像收进来。
// 不能过的（站内链接、外站域名、私有地址）直接跳过，不报错：
// 页面上本来就混着几十条导航链接，逐条报错除了刷日志没有别的用处。
func scanMirrorItems(section string) []mirrorEntry {
	var out []mirrorEntry
	seen := map[string]bool{}
	for _, li := range reListItem.FindAllStringSubmatch(section, -1) {
		href := ""
		if h := reHref.FindStringSubmatch(li[1]); len(h) == 2 {
			href = h[1]
		}
		if href == "" {
			continue
		}
		base, _, _, err := validateMirrorBase(href, nil)
		if err != nil {
			continue
		}
		if seen[base] {
			continue
		}
		seen[base] = true
		loc := ""
		if m := reLocation.FindStringSubmatch(li[1]); len(m) == 2 {
			loc = cleanMirrorLocation(m[1])
		}
		out = append(out, mirrorEntry{Base: base, Location: loc})
	}
	return out
}

// scanMirrorLinks 是全页兵底：不要求链接在 <li> 里，只收 href。
// 只当 Label1 那块完全解析不出东西时才用。
func scanMirrorLinks(body string) []mirrorEntry {
	var out []mirrorEntry
	seen := map[string]bool{}
	for _, m := range reHref.FindAllStringSubmatch(body, -1) {
		base, _, _, err := validateMirrorBase(m[1], nil)
		if err != nil || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, mirrorEntry{Base: base})
	}
	return out
}

// mirrorSubnet 返回镜像所在的 /24。
//
// 按 /24 去重比按 location 更准：实测列表页上 5 个克罗地亚镜像全在
// 150.40.105.0/24，其中 .16 与 .13、.6 与 .12 的节点 CSV 逐字节相同
// —— 同一网段基本就是同一处机房的多个端口，重复拉只是白花带宽。
func mirrorSubnet(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.To4().Mask(net.CIDRMask(24, 32)).String()
}

// pickMirrorSources 从缓存的镜像列表里挑 n 个彼此不同源的镜像。
//
// 两条去重规则都要：/24 相同视为同一处机房，location 相同视为同一地区。
// rotation 是轮转偏移 —— 每轮换一批，跨轮次的并集会持续变大，
// 而每轮的成本恒定。
func pickMirrorSources(entries []mirrorEntry, n int, rotation int) []mirrorEntry {
	if n <= 0 || len(entries) == 0 {
		return nil
	}
	out := make([]mirrorEntry, 0, n)
	usedSubnet := map[string]bool{}
	usedLocation := map[string]bool{}
	start := rotation % len(entries)
	if start < 0 {
		start += len(entries)
	}
	for i := 0; i < len(entries) && len(out) < n; i++ {
		e := entries[(start+i)%len(entries)]
		if strings.TrimSpace(e.Base) == "" {
			continue
		}
		if s := mirrorSubnet(e.Base); s != "" {
			if usedSubnet[s] {
				continue
			}
			usedSubnet[s] = true
		}
		if loc := locationKey(e.Location); loc != "" {
			if usedLocation[loc] {
				continue
			}
			usedLocation[loc] = true
		}
		out = append(out, e)
	}
	return out
}

// feedURL 拼出某个源的节点列表接口。
// 实测 6 个镜像的路径完全一致：http://IP:PORT/api/iphone/
func feedURL(base string) string {
	return strings.TrimRight(base, "/") + "/api/iphone/"
}

// mirrorListStale 判断缓存的镜像列表是否该重拉了。
func mirrorListStale(fetched time.Time, maxAge time.Duration) bool {
	if fetched.IsZero() {
		return true
	}
	return time.Since(fetched) > maxAge
}

// mirrorFailBackoff 是列表页连续失败后的重试间隔。
// 上电那次失败尤其不能“一失败就静默等 12 小时”，那等于首整天只有单源。
var mirrorFailBackoff = []time.Duration{
	15 * time.Minute, time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// mirrorsPerRound 每轮取几个镜像。实测 2 个就到收益拐点（再多只有个位数增益）；
// 设 0 等于关掉多源，只用主站 + 自建反代（即改动前的行为）。
func mirrorsPerRound() int {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_SOURCES"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= maxMirrorEntries {
			return n
		}
	}
	return maxMirrorsPerFetch
}

// mirrorStore 管镜像列表的读盘缓存、过期重拉、失败退避、轮转偏移。
//
// 它只在刷新节点列表时被碰，而刷新可能来自 ticker、重连退避、手动接口，
// 所以自己带一把锁。
type mirrorStore struct {
	mu       sync.Mutex
	dir      string
	entries  []mirrorEntry
	fetched  time.Time // 列表页上次成功时间（不是 CSV 的）
	rotation int       // 轮转偏移，每轮 +1
	fails    int
	nextTry  time.Time
	// portBusy 判断端口是否已被占用。做成字段是为了测试里能绕开真 bind，
	// 不去依赖CI 机器的端口占用情况。
	portBusy func(int) bool
}

// newMirrorStore 先读盘：重启时不必等网络就有一份可用的镜像列表。
func newMirrorStore(dir string) *mirrorStore {
	s := &mirrorStore{dir: dir, portBusy: listeningPort}
	entries, fetched, err := loadMirrors(dir)
	if err != nil {
		log.Printf("读取镜像列表缓存失败（会重拉）: %v", err)
	}
	s.entries, s.fetched = entries, fetched
	return s
}

// listeningPort 用“能不能 bind”判断端口是否已被占用。
//
// 要这项检查是因为：镜像列表页给的是公网 IP，回环与私有段已经被地址校验
// 拦了，但“本机公网 IP : 管理端口 / 已分配的 SOCKS5 端口”这种形式进得来。
// 真去连一个自己开的端口，拿到的是自己的服务，解析不了倒还算轻的。
func listeningPort(port int) bool {
	return !portAvailable(port)
}

// sources 返回本轮要拉的源：主站在第 0 位，后面是最多 mirrorsPerRound() 个镜像。
// 列表过期或还没有时先重拉；拉不动就沿用旧列表并按退避跳过。
func (s *mirrorStore) sources(now time.Time, timeout time.Duration) []string {
	urls := []string{vpngateAPI}
	n := mirrorsPerRound()
	if n <= 0 {
		return urls
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 || mirrorListStale(s.fetched, mirrorListMaxAge) {
		// 顺序不能倒：先判退避，否则退避期到了之前每轮都会白打一次列表页。
		// （写成 switch init 的形式就踩过这个坑：init 比 case 先执行。）
		if now.Before(s.nextTry) {
			log.Printf("镜像列表页还在退避中（%s 后重试），本轮只用主站", s.nextTry.Sub(now).Round(time.Minute))
		} else if entries, err := fetchMirrorList(timeout); err != nil {
			s.fails++
			wait := mirrorFailBackoff[min(s.fails-1, len(mirrorFailBackoff)-1)]
			s.nextTry = now.Add(wait)
			log.Printf("拉取镜像列表页失败（连续 %d 次，%s 后重试）: %v", s.fails, wait, err)
		} else {
			s.entries, s.fetched = entries, now
			s.fails, s.nextTry = 0, time.Time{}
			if werr := saveMirrors(s.dir, entries); werr != nil {
				log.Printf("保存镜像列表缓存失败: %v", werr)
			}
			log.Printf("镜像列表已更新：%d 个", len(entries))
		}
	}

	picked := make([]mirrorEntry, 0, n)
	for _, e := range pickMirrorSources(s.entries, n, s.rotation) {
		// 解析阶段不查端口（页面上几十条链接逐条 bind 一次不现实），
		// 真正要拉之前这一道必须过。
		busy := listeningPort
		if s.portBusy != nil {
			busy = s.portBusy
		}
		base, _, _, err := validateMirrorBase(e.Base, busy)
		if err != nil {
			log.Printf("跳过镜像 %s: %v", e.Base, err)
			continue
		}
		picked = append(picked, mirrorEntry{Base: base, Location: e.Location})
	}
	s.rotation++
	for _, e := range picked {
		urls = append(urls, feedURL(e.Base))
	}
	return urls
}

// summary 给日志与界面用：当前缓存的镜像数与列表页上次更新时间。
func (s *mirrorStore) summary() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.fetched
}

// noteFailure 记一次“整轮节点源都失败”，让下一轮更早重拉列表页。
// 列表页本身没坏，但源全挂通常意味着网络出问题了，值得早点重试。
func (s *mirrorStore) noteFailure(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextTry.IsZero() || s.nextTry.Before(now) {
		s.nextTry = now.Add(15 * time.Minute)
	}
}

type mirrorCache struct {
	Fetched time.Time     `json:"fetched"`
	Mirrors []mirrorEntry `json:"mirrors"`
}

func mirrorsPath(dir string) string { return filepath.Join(dir, "mirrors.json") }

// loadMirrors 读缓存。文件不存在不算错误（返回空列表），
// 内容坏了则返回错误，由调用方决定是记日志后忽略还是重拉。
func loadMirrors(dir string) ([]mirrorEntry, time.Time, error) {
	blob, err := os.ReadFile(mirrorsPath(dir))
	if os.IsNotExist(err) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var c mirrorCache
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, time.Time{}, fmt.Errorf("解析镜像列表缓存失败: %w", err)
	}
	return c.Mirrors, c.Fetched, nil
}

func saveMirrors(dir string, entries []mirrorEntry) error {
	blob, err := json.MarshalIndent(mirrorCache{Fetched: time.Now().UTC(), Mirrors: entries}, "", "  ")
	if err != nil {
		return err
	}
	path := mirrorsPath(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return fmt.Errorf("写镜像列表缓存失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// fetchMirrorList 拉镜像列表页并解析出可用镜像。
// 调用方负责决定失败时怎么办（沿用旧缓存 / 退避重试）。
func fetchMirrorList(timeout time.Duration) ([]mirrorEntry, error) {
	page := sitesPageURL()
	if page == "" {
		return nil, fmt.Errorf("镜像列表页已被禁用（FANOUT_VPNGATE_SITES 为空）")
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: nodeTransport, // 见 nodeTransport 的注释：不关掉 gzip 协商会挂死
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("镜像列表页重定向次数过多")
			}
			return nil
		},
	}
	resp, err := client.Get(page)
	if err != nil {
		return nil, fmt.Errorf("拉取镜像列表页失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取镜像列表页失败: HTTP %d", resp.StatusCode)
	}
	// 多读 1 字节用来判断"是否超限"，不能直接 LimitReader 到上限就完事：
	// 截断后的页面照样能解析出几个镜像，那种半截结果比失败更危险。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, sitesPageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取镜像列表页失败: %w", err)
	}
	if len(raw) > sitesPageMaxBytes {
		return nil, fmt.Errorf("镜像列表页超过 %d 字节，按异常处理", sitesPageMaxBytes)
	}
	if len(raw) < 512 {
		return nil, fmt.Errorf("镜像列表页内容过短(%d 字节)，按异常处理", len(raw))
	}
	entries, err := parseMirrorPage(string(raw))
	if err != nil {
		return nil, err
	}
	// 没有 location 时不做地区去重，只靠 /24。不是错误，但要让人看得见。
	hasLocation := false
	for _, e := range entries {
		if e.Location != "" {
			hasLocation = true
			break
		}
	}
	if !hasLocation {
		log.Printf("镜像列表页没有 Mirror location 字段，选源时只按网段去重")
	}
	return entries, nil
}
