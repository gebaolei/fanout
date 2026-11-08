package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 第 4 类源：第三方维护的 VPN Gate 滚动池（GitHub + CDN）。
//
// 为什么要有它：源站的 CSV 只给按分数排的头部 ~100 个节点，家宽只有 ~80 个、
// 国家十来个，所以"换个节点"翻来覆去就那几个 IP。这个池子把最近 7 天在源站
// 出现过的节点全部留着（4900+ 条），而且自己养了两个 Cloudflare Worker 做
// TCP 探测 —— 实测 status=reachable 且有 6 小时内的探测记录的家宽节点有 640 个，
// 覆盖 24 个国家，其中绝大多数此刻不在源站 CSV 里。
//
// 它与前面三类源的差别（重要）：
//
//	源站/官方镜像  实时清单，.ovpn 就在 CSV 那一列里，会被限流
//	本池           30 分钟一轮的静态副本，不会被限流；.ovpn 按 sha256 分文件存，
//	               要连哪个节点时才去取（13KB 一个）；探测只到 TCP 端口层面，
//	               而且是从 Cloudflare 边缘探的，不代表从这台机器能连上
//
// 所以这里的节点只当候选来源用，可用与否仍然由 fanout 自己的
// OpenVPN 握手 + probeExitIP + 健康检查决定。
const (
	// 索引文件很小（1KB 上下），里面是每个文件的 sha256 和 data_commit。
	poolIndexMaxBytes = 256 << 10
	// 池文件实测 7.1MB。上游自己声明的上限是 16 MiB，就按它来；
	// 再大说明对方改了格式，宁可不用也不能拿半截数据当列表。
	poolServersMaxBytes = 16 << 20
	// 单个节点的 .ovpn 文件：JSON 包一层，里面是 base64，约 13.5KB。
	poolConfigMaxBytes = 512 << 10
	// 探测结果多久算旧。上游给消费者的建议是 3 小时算 stale、24 小时算 very
	// stale；这里取 6 小时：家宽节点开关机很常见，太旧的"能连上"没什么参考价值，
	// 太严又白丢一半候选。
	poolProbeMaxAge = 6 * time.Hour
	// 池文件多久重拉一次。上游每 30 分钟才发一轮（Cloudflare Cron 在 29、59 分
	// 触发 Actions），拉得更勤只会白花 7MB 带宽。
	poolRefreshInterval = 30 * time.Minute
	// 缓存能顶多久。超过这个时间还拉不动，就不再把老池子当候选来源
	// （节点会一路变旧，最后全是死节点），但也不会因此报错——节点列表刷新
	// 本身还有主站和镜像兜着。
	poolCacheMaxAge = 24 * time.Hour
	// 最多收多少个。全池 4900 条都收进来没必要：挑节点时同地区取前几个就够，
	// 而 m.nodes 会在界面和订阅里被遍历。按"探测最新"截断到这么多。
	poolMaxNodes = 1200
)

// poolMinUsable：过滤后至少要有多少个才认为这次拉取正常。
// 格式变了、探测数据没了都会让数量骤降到个位数，那种列表不如不用。
//
// 写成 var（不是 const）只是为了单测能拿几个节点跑通整条路径。
var poolMinUsable = 20

// poolRepoRef 是仓库标识，只用来拼下载地址。
const poolRepoRef = "GeorgeXie2333/vpngate-list-mirror"

// 下载地址可以整体换掉（自建镜像、国内加速、CI 里指向 httptest）。
// 设成空字符串＝彻底不用这个源。
var (
	poolIndexDefault = "https://raw.githubusercontent.com/" + poolRepoRef + "/main/pool/latest.json"
	// jsDelivr 在前：它的 commit 路径是不可变缓存，比 raw 快也不容易被限流。
	poolCDNBaseDefault = "https://cdn.jsdelivr.net/gh/" + poolRepoRef
	poolRawBaseDefault = "https://raw.githubusercontent.com/" + poolRepoRef
)

func poolIndexURL() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_POOL"); ok {
		return strings.TrimSpace(v)
	}
	return poolIndexDefault
}

func poolCDNBase() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_POOL_CDN"); ok {
		return strings.TrimSpace(v)
	}
	return poolCDNBaseDefault
}

func poolRawBase() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_POOL_RAW"); ok {
		return strings.TrimSpace(v)
	}
	return poolRawBaseDefault
}

// poolFileURLs 返回某个文件的下载地址，按优先级排。
// jsDelivr 的 @commit 与 GitHub Raw 的 /commit/ 指的是同一份内容，
// 前一个失败就换后一个（同一个 commit，不会拿到两份不同的数据）。
var poolFileURLs = func(commit, path string) []string {
	return []string{
		strings.TrimRight(poolCDNBase(), "/") + "@" + commit + "/" + path,
		strings.TrimRight(poolRawBase(), "/") + "/" + commit + "/" + path,
	}
}

// poolFetchTimeout 是拉池文件的超时。7.1MB 比节点 CSV 大得多，
// 25 秒那个默认值在慢链路上不够用。
func poolFetchTimeout() time.Duration {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_POOL_TIMEOUT"); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

// poolIndex 是索引文件（pool/latest.json）。
//
// 用法是它规定的：先读索引、把 data_commit 固定下来，之后所有文件都取
// 那个 commit，并校验字节数与 sha256。@latest/@main 不作数（CDN 缓存 7 天）。
type poolIndex struct {
	Kind            string `json:"kind"`
	SchemaVersion   int    `json:"schema_version"`
	DataCommit      string `json:"data_commit"`
	ServerCount     int    `json:"server_count"`
	SourceFetchedAt string `json:"source_fetched_at"`
	Files           map[string]struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

const poolServersPath = "pool/servers.json"

// poolEntry 是池文件里的一个节点。只声明用得上的字段，
// 上游加字段不影响解析（encoding/json 会忽略多余的）。
type poolEntry struct {
	Hostname    string `json:"hostname"`
	IP          string `json:"ip"`
	CountryCode string `json:"country_code"`
	CountryName string `json:"country_name"`
	PingMS      int    `json:"ping_ms"`
	SpeedBPS    int64  `json:"speed_bps"`
	Sessions    int    `json:"num_vpn_sessions"`
	// PresentInLatestSource 表示这一时刻它还在源站 CSV 里。
	// 不做硬性要求 —— 增量恰恰来自"不在 CSV 里但最近探过"的那些。
	PresentInLatestSource bool   `json:"present_in_latest_source"`
	LastSeenAt            string `json:"last_seen_at"`
	TCPProbe              struct {
		Status              string `json:"status"`
		CheckedAt           string `json:"checked_at"`
		LastSuccessAt       string `json:"last_success_at"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
	} `json:"tcp_probe"`
	Config struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	} `json:"config"`
}

type poolServers struct {
	Kind            string      `json:"kind"`
	SchemaVersion   int         `json:"schema_version"`
	ServerCount     int         `json:"server_count"`
	SourceFetchedAt string      `json:"source_fetched_at"`
	Servers         []poolEntry `json:"servers"`
}

// poolConfigFile 是单个节点的 .ovpn 文件。
//
// 实测 SHAPlain 是对解码后的 .ovpn 明文求的 sha256（也正好是文件名），
// 不是对 base64 文本求的（两者实测不同，这里踩过）。
// Base64SHA 现在上游没发，留着：哪天发了就顺便多验一道。
type poolConfigFile struct {
	Kind                string `json:"kind"`
	OpenVPNConfigBase64 string `json:"openvpn_config_base64"`
	SHAPlain            string `json:"openvpn_config_sha256"`
	Base64SHA           string `json:"openvpn_config_base64_sha256"`
	OpenVPNConfigBytes  int    `json:"openvpn_config_bytes"`
}

// poolProbeFresh 判断探测记录还新不新。
// 上游在 checked_at 缺失的老记录上会退回用别的时间，这里也一样：
// 先看 checked_at（最近一次探过，含失败），没有就看 last_success_at。
func poolProbeFresh(e poolEntry, now time.Time, maxAge time.Duration) bool {
	for _, s := range []string{e.TCPProbe.CheckedAt, e.TCPProbe.LastSuccessAt} {
		if s == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			continue
		}
		// 要求的是“at 不早于 now-maxAge”。写成 at <= now+maxAge 会恒真，
		// 新鲜度过滤等于没做（踩过，单测拓出来的）。
		if !at.Before(now.Add(-maxAge)) {
			return true
		}
	}
	// 两个时间都读不出来时按"旧"处理：宁可少用，不要用不知多久的
	return false
}

// poolUsable 判断一个条目能不能当候选。
//
// reachable 只表示"最近从 Cloudflare 边缘 TCP 连上过这个 ip:port"，
// 不是 VPN 握手成功，也不代表从这台机器通 —— 那由 fanout 自己验。
func poolUsable(e poolEntry, now time.Time) bool {
	if strings.TrimSpace(e.Hostname) == "" || strings.TrimSpace(e.IP) == "" {
		return false
	}
	// .ovpn 不在列表里，必须知道去哪取
	if strings.TrimSpace(e.Config.Path) == "" || len(e.Config.SHA256) != 64 {
		return false
	}
	if e.TCPProbe.Status != "reachable" {
		return false
	}
	// reachable 时这个应当就是 0，但上游也说过轮次断裂会重置，双保险
	if e.TCPProbe.ConsecutiveFailures > 0 {
		return false
	}
	return poolProbeFresh(e, now, poolProbeMaxAge)
}

// poolToNode 把池条目变成 Node。Config 留空：那 13KB 要到真要连它时再去取。
func poolToNode(e poolEntry, commit string) Node {
	return Node{
		HostName:    e.Hostname,
		IP:          e.IP,
		Residential: isResidential(e.Hostname, e.IP),
		Country:     e.CountryName,
		CountryCode: strings.ToUpper(strings.TrimSpace(e.CountryCode)),
		Ping:        e.PingMS,
		SpeedMbps:   float64(e.SpeedBPS) / 1e6,
		Sessions:    e.Sessions,
		RemoteConfig: &configRef{
			Commit: commit,
			Path:   e.Config.Path,
			SHA256: e.Config.SHA256,
		},
	}
}

// filterPoolServers 过滤 + 截断，返回留下的条目与对应的 Node。
//
// 按"探测最新"排序，这样超出 poolMaxNodes 时先扔的是探测最旧的那些
// （而不是恰好按国家或速度丢了一批旧的）。同时返回条目是为了能把
// 真正的探测时间落盘，重启后还能判新旧。
func filterPoolServers(list []poolEntry, commit string, now time.Time) ([]poolEntry, []Node) {
	fresh := make([]poolEntry, 0, len(list))
	for _, e := range list {
		if poolUsable(e, now) {
			fresh = append(fresh, e)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool {
		return probeAge(fresh[i], now) < probeAge(fresh[j], now)
	})
	if len(fresh) > poolMaxNodes {
		fresh = fresh[:poolMaxNodes]
	}
	out := make([]Node, 0, len(fresh))
	for _, e := range fresh {
		out = append(out, poolToNode(e, commit))
	}
	return fresh, out
}

// probeAge 返回探测记录距今多久，读不出来按"很旧"。
func probeAge(e poolEntry, now time.Time) time.Duration {
	for _, s := range []string{e.TCPProbe.CheckedAt, e.TCPProbe.LastSuccessAt} {
		if s == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		if err == nil {
			return now.Sub(at)
		}
	}
	return 1000 * time.Hour
}

// poolStore 管这个源的索引、池文件缓存、失败退避，以及按需取的 .ovpn。
//
// 和 mirrorStore 一样：刷新可能来自 ticker、重连退避、手动接口，
// 所以自己带锁；任何失败都不影响别的源。
type poolStore struct {
	mu  sync.Mutex
	dir string

	commit   string    // 当前缓存内容对应的 commit
	sha256   string    // 当前缓存的池文件 sha256
	fetched  time.Time // 上次成功拉取（或确认没变化）的时间
	sourceAt time.Time // 上游抓源站的时间（界面显示新鲜度用）
	entries  []poolEntry
	nodes    []Node

	configs map[string]string // commit/path → 解码后的 .ovpn
	fails   int
	nextTry time.Time
}

func newPoolStore(dir string) *poolStore {
	s := &poolStore{dir: dir, configs: map[string]string{}}
	if entries, commit, sourceAt, err := loadPool(dir); err != nil {
		log.Printf("读取节点池缓存失败（会重拉）: %v", err)
	} else if len(entries) > 0 {
		s.entries = entries
		s.commit = commit
		// sourceAt 一并接回来：重启后界面要能看出这份列表是什么时候的数据，
		// 不该因为重启变成“未知”。fetched 故意不接：重启后应该去索引那
		// 1KB 确认一下有没有新版本（commit 没变就不会重拉那 7.1MB）。
		s.sourceAt = sourceAt
		_, s.nodes = filterPoolServers(entries, commit, time.Now())
	}
	return s
}

// poolCache 是落盘的形态：只存过滤后的条目，几十到几百 KB，
// 重启后不必重新下载 7.1MB。
type poolCache struct {
	Commit        string      `json:"commit"`
	Fetched       time.Time   `json:"fetched"`
	SourceFetched time.Time   `json:"source_fetched_at"`
	Servers       []poolEntry `json:"servers"`
}

func poolCachePath(dir string) string { return filepath.Join(dir, "pool.json") }

func loadPool(dir string) ([]poolEntry, string, time.Time, error) {
	blob, err := os.ReadFile(poolCachePath(dir))
	if os.IsNotExist(err) {
		return nil, "", time.Time{}, nil
	}
	if err != nil {
		return nil, "", time.Time{}, err
	}
	var c poolCache
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("解析节点池缓存失败: %w", err)
	}
	return c.Servers, c.Commit, c.SourceFetched, nil
}

func savePool(dir string, c poolCache) error {
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := poolCachePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return fmt.Errorf("写节点池缓存失败: %w", err)
	}
	return os.Rename(tmp, poolCachePath(dir))
}

// get 返回这一轮可用的池内节点，并顺手记下耗时（与 CSV 源同一个口径，
// 界面上的“各源结果”才能横比）。
func (s *poolStore) get(now time.Time, timeout time.Duration) ([]Node, sourceStat, error) {
	t0 := time.Now()
	nodes, st, err := s.load(now, timeout)
	st.MS = time.Since(t0).Milliseconds()
	return nodes, st, err
}

// load 才是真的在读。
//
// 返回 (nodes, stat, err)：nodes 为空时调用方直接用别的源，不算失败。
// stat 与多源聚合共用一个结构，这样界面和日志不用特判。
//
// 关键点：只有 commit 变了才会重新下载那 7.1MB。索引只有 1KB，
// 上游每 30 分钟才发一轮，所以同一轮里重复问索引是几乎免费的。
func (s *poolStore) load(now time.Time, timeout time.Duration) ([]Node, sourceStat, error) {
	indexURL := poolIndexURL()
	st := sourceStat{URL: indexURL}
	if indexURL == "" {
		return nil, st, nil // 被关掉了：不是错误，也不参与统计
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.nodes) > 0 && now.Sub(s.fetched) < poolRefreshInterval {
		st.Nodes = len(s.nodes)
		return s.nodes, st, nil
	}
	if now.Before(s.nextTry) {
		// 还在退避期：有旧缓存就用旧的，没有就当这个源没贡献
		if len(s.nodes) > 0 && now.Sub(s.fetched) < poolCacheMaxAge {
			st.Nodes = len(s.nodes)
			return s.nodes, st, nil
		}
		return s.nodes, st, fmt.Errorf("节点池源在退避中（%s 后重试）", s.nextTry.Sub(now).Round(time.Minute))
	}

	idx, err := fetchPoolIndex(indexURL, timeout)
	if err != nil {
		s.noteFailureLocked(now)
		return s.nodes, st, err
	}
	meta, ok := idx.Files[poolServersPath]
	if !ok {
		s.noteFailureLocked(now)
		return s.nodes, st, fmt.Errorf("节点池索引里没有 %s", poolServersPath)
	}
	if idx.DataCommit == s.commit && idx.DataCommit != "" && len(s.entries) > 0 {
		// 上游没出新内容，不必再拉 7MB
		s.fetched = now
		st.Nodes = len(s.nodes)
		if len(s.nodes) == 0 {
			// 缓存里全是通过期了（机器一整天没联上网就会这样）。
			// 重拉同一个 commit 也一样旧，所以报错而不是当作可用。
			return s.nodes, st, fmt.Errorf("节点池缓存已超过 %d 小时未探测", int(poolProbeMaxAge.Hours()))
		}
		return s.nodes, st, nil
	}

	list, err := fetchPoolServers(idx, timeout)
	if err != nil {
		s.noteFailureLocked(now)
		return s.nodes, st, err
	}
	kept, nodes := filterPoolServers(list, idx.DataCommit, now)
	if len(nodes) < poolMinUsable {
		s.noteFailureLocked(now)
		return s.nodes, st, fmt.Errorf("节点池可用节点过少(%d)，按异常处理", len(nodes))
	}
	s.commit, s.sha256 = idx.DataCommit, meta.SHA256
	s.entries, s.nodes = kept, nodes
	s.fetched = now
	s.fails, s.nextTry = 0, time.Time{}
	if at, err := time.Parse(time.RFC3339Nano, idx.SourceFetchedAt); err == nil {
		s.sourceAt = at
	}
	if err := savePool(s.dir, poolCache{
		Commit:        s.commit,
		Fetched:       now.UTC(),
		SourceFetched: s.sourceAt.UTC(),
		Servers:       kept,
	}); err != nil {
		log.Printf("保存节点池缓存失败: %v", err)
	}
	st.Nodes = len(nodes)
	log.Printf("节点池已更新：commit %.10s，%d/%d 个节点可用（探测 %d 小时内）",
		s.commit, len(nodes), idx.ServerCount, int(poolProbeMaxAge.Hours()))
	return nodes, st, nil
}

func (s *poolStore) noteFailureLocked(now time.Time) {
	s.fails++
	wait := mirrorFailBackoff[min(s.fails-1, len(mirrorFailBackoff)-1)]
	s.nextTry = now.Add(wait)
}

// summary 给界面用：缓存多少个节点、上游最后一次抓源站是什么时候。
func (s *poolStore) summary() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes), s.sourceAt
}

// fetchPoolIndex 读索引。只有 1KB 左右，但照样要限大小并校验格式。
func fetchPoolIndex(url string, timeout time.Duration) (*poolIndex, error) {
	raw, err := poolGet(url, poolIndexMaxBytes, timeout)
	if err != nil {
		return nil, fmt.Errorf("拉取节点池索引失败: %w", err)
	}
	var idx poolIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("解析节点池索引失败: %w", err)
	}
	// kind/schema 不对说明上游改了格式。这时候宁可报错也别按老结构硬解：
	// 字段名一改名就全空，池子会"安静地"变成 0 个可用节点。
	if idx.Kind != "" && idx.Kind != "vpngate-pool" {
		return nil, fmt.Errorf("节点池索引 kind=%q，不是预期的 vpngate-pool", idx.Kind)
	}
	if idx.SchemaVersion != 1 {
		return nil, fmt.Errorf("节点池索引 schema_version=%d，本程序只认 1", idx.SchemaVersion)
	}
	if len(idx.DataCommit) != 40 {
		return nil, fmt.Errorf("节点池索引 data_commit 长度异常: %q", idx.DataCommit)
	}
	if idx.ServerCount > 20000 || idx.ServerCount <= 0 {
		return nil, fmt.Errorf("节点池索引 server_count=%d 不合理", idx.ServerCount)
	}
	return &idx, nil
}

// fetchPoolServers 按索引给的 commit 拉池文件，并校验字节数与 sha256。
// 上游明确要求：对收到的原始字节求 sha256，不要对重新序列化后的 JSON 求。
func fetchPoolServers(idx *poolIndex, timeout time.Duration) ([]poolEntry, error) {
	meta := idx.Files[poolServersPath]
	var lastErr error
	for _, url := range poolFileURLs(idx.DataCommit, poolServersPath) {
		raw, err := poolGet(url, poolServersMaxBytes, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		if meta.Bytes > 0 && len(raw) != meta.Bytes {
			lastErr = fmt.Errorf("节点池文件 %d 字节，索引说 %d 字节", len(raw), meta.Bytes)
			continue
		}
		if meta.SHA256 != "" {
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != strings.ToLower(meta.SHA256) {
				// 校验失败通常是 CDN 给了坏内容，换下一个源还有意义
				lastErr = fmt.Errorf("节点池文件 sha256 不符（要 %s，得 %s）", meta.SHA256, got)
				continue
			}
		}
		var servers poolServers
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, fmt.Errorf("解析节点池文件失败: %w", err)
		}
		if len(servers.Servers) == 0 {
			return nil, fmt.Errorf("节点池文件里没有节点")
		}
		if servers.SchemaVersion != 0 && servers.SchemaVersion != 1 {
			return nil, fmt.Errorf("节点池文件 schema_version=%d，本程序只认 1", servers.SchemaVersion)
		}
		return servers.Servers, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的下载地址")
	}
	return nil, fmt.Errorf("拉取节点池文件失败: %w", lastErr)
}

// config 按需取一个节点的 .ovpn。commit 用节点上记的那个，
// 否则列表是新 commit、config 是旧 commit，会对不上。
func (s *poolStore) config(n Node, timeout time.Duration) (string, error) {
	ref := n.RemoteConfig
	if ref == nil || ref.Path == "" {
		return "", fmt.Errorf("节点 %s 没有可取的配置引用", n.HostName)
	}
	key := ref.Commit + "/" + ref.Path

	s.mu.Lock()
	if cached, ok := s.configs[key]; ok {
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	var lastErr error
	for _, url := range poolFileURLs(ref.Commit, ref.Path) {
		raw, err := poolGet(url, poolConfigMaxBytes, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		var f poolConfigFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", fmt.Errorf("解析节点 %s 的配置失败: %w", n.HostName, err)
		}
		cfg, err := base64.StdEncoding.DecodeString(strings.TrimSpace(f.OpenVPNConfigBase64))
		if err != nil {
			return "", fmt.Errorf("节点 %s 的配置不是合法 base64: %w", n.HostName, err)
		}
		want := strings.ToLower(firstNonEmpty(f.SHAPlain, ref.SHA256))
		if len(want) == 64 {
			sum := sha256.Sum256(cfg)
			if got := hex.EncodeToString(sum[:]); got != want {
				// 校验失败通常是 CDN 给了坏内容，换一个地址再试
				lastErr = fmt.Errorf("节点 %s 配置 sha256 不符（要 %s，得 %s）", n.HostName, want, got)
				continue
			}
		}
		if b64sha := strings.ToLower(strings.TrimSpace(f.Base64SHA)); len(b64sha) == 64 {
			sum := sha256.Sum256([]byte(strings.TrimSpace(f.OpenVPNConfigBase64)))
			if got := hex.EncodeToString(sum[:]); got != b64sha {
				lastErr = fmt.Errorf("节点 %s 的 base64 sha256 不符（要 %s，得 %s）", n.HostName, b64sha, got)
				continue
			}
		}
		if f.OpenVPNConfigBytes > 0 && f.OpenVPNConfigBytes != len(cfg) {
			lastErr = fmt.Errorf("节点 %s 配置 %d 字节，文件里写着 %d 字节", n.HostName, len(cfg), f.OpenVPNConfigBytes)
			continue
		}
		text := string(cfg)
		// 空配置或明显不是 .ovpn 的内容都不要：openvpn 会等到超时才报错
		if len(cfg) < 200 || !strings.Contains(text, "remote") {
			return "", fmt.Errorf("节点 %s 的配置内容不对（%d 字节）", n.HostName, len(cfg))
		}
		s.mu.Lock()
		s.rememberConfigLocked(key, text)
		s.mu.Unlock()
		return text, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的下载地址")
	}
	return "", fmt.Errorf("拉取节点配置失败: %w", lastErr)
}

// rememberConfigLocked 存一份已解码配置。只存最近这些条：
// 一条出口一辈子也就连几十个节点，全存进内存没必要（每条 10KB）。
func (s *poolStore) rememberConfigLocked(key, text string) {
	if s.configs == nil {
		s.configs = map[string]string{}
	}
	if len(s.configs) >= 64 {
		for k := range s.configs {
			delete(s.configs, k)
			if len(s.configs) < 32 {
				break
			}
		}
	}
	s.configs[key] = text
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// poolGet 拉一个 URL 并限制大小。多读 1 字节判断超限，
// 不能直接截断：半截 JSON 照样能解析出东西，那种结果比失败更危险。
func poolGet(url string, maxBytes int64, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout, Transport: nodeTransport}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("内容超过 %d 字节，按异常处理", maxBytes)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("返回空内容")
	}
	return raw, nil
}
