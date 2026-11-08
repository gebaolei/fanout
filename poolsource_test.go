package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 构造一个池条目。默认“刚刚探测过且通”。
type poolOpt func(*poolEntry)

func poolEntryNamed(host string, opts ...poolOpt) poolEntry {
	e := poolEntry{
		Hostname:    host,
		IP:          fmt.Sprintf("203.0.113.%d", len(host)%250+1),
		CountryCode: "jp",
		CountryName: "Japan",
		PingMS:      20,
		SpeedBPS:    50 << 20,
		Config: struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Bytes  int    `json:"bytes"`
		}{Path: "pool/configs/" + host + ".json", SHA256: strings.Repeat("a", 64)},
	}
	e.TCPProbe.Status = "reachable"
	e.TCPProbe.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	for _, o := range opts {
		o(&e)
	}
	return e
}

func poolProbeAt(t time.Time) poolOpt {
	return func(e *poolEntry) {
		e.TCPProbe.CheckedAt = t.UTC().Format(time.RFC3339Nano)
		e.TCPProbe.LastSuccessAt = e.TCPProbe.CheckedAt
	}
}

func poolStatus(s string) poolOpt {
	return func(e *poolEntry) {
		e.TCPProbe.Status = s
		e.TCPProbe.ConsecutiveFailures = 0
		if s != "reachable" {
			e.TCPProbe.ConsecutiveFailures = 1
		}
	}
}

func poolNoConfig() poolOpt {
	return func(e *poolEntry) { e.Config.Path = "" }
}

func TestPoolUsable(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		e    poolEntry
		want bool
	}{
		{"正常可达", poolEntryNamed("vpn100"), true},
		{"探不到", poolEntryNamed("vpn101", poolStatus("unreachable")), false},
		{"没探过", poolEntryNamed("vpn102", poolStatus("unknown")), false},
		{"不适用（UDP 等）", poolEntryNamed("vpn103", poolStatus("not_applicable")), false},
		{"探测记录超过 6 小时", poolEntryNamed("vpn104", poolProbeAt(now.Add(-7*time.Hour))), false},
		{"刚好在窗口内", poolEntryNamed("vpn105", poolProbeAt(now.Add(-5*time.Hour))), true},
		{"没有 config 路径", poolEntryNamed("vpn106", poolNoConfig()), false},
		{"缺 hostname", func() poolEntry { e := poolEntryNamed("vpn107"); e.Hostname = ""; return e }(), false},
		{"连败不为 0", func() poolEntry {
			e := poolEntryNamed("vpn108")
			e.TCPProbe.ConsecutiveFailures = 2
			return e
		}(), false},
		// 时间读不出来按旧的算：宁可少用，不要用不知多久的
		{"时间格式坏了", func() poolEntry {
			e := poolEntryNamed("vpn109")
			e.TCPProbe.CheckedAt = "昨天"
			e.TCPProbe.LastSuccessAt = ""
			return e
		}(), false},
	}
	for _, c := range cases {
		if got := poolUsable(c.e, now); got != c.want {
			t.Errorf("%s: poolUsable=%v，想要 %v", c.name, got, c.want)
		}
	}
}

// 地区要转成大写：CSV 那边给的是大写，挑节点按 EqualFold 比，
// 但界面显示和 countryLabel 依赖大写形态。
func TestPoolToNode(t *testing.T) {
	e := poolEntryNamed("vpn210075524")
	e.CountryCode = "kr"
	e.Sessions = 7
	n := poolToNode(e, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	if n.CountryCode != "KR" {
		t.Errorf("地区应转大写，实际 %q", n.CountryCode)
	}
	if n.Config != "" {
		t.Error(".ovpn 不该在列表里就填上，要等真要连时再取")
	}
	if n.RemoteConfig == nil || n.RemoteConfig.Path != e.Config.Path || n.RemoteConfig.Commit == "" {
		t.Errorf("缺少配置引用: %+v", n.RemoteConfig)
	}
	if !n.Residential {
		t.Error("vpn<数字> 应算志愿者家宽")
	}
	if n.SpeedMbps != float64(e.SpeedBPS)/1e6 {
		t.Errorf("速度换算不对: %v", n.SpeedMbps)
	}
	// 机房节点在池子里也有，占比小但要能标出来（"只用家宽"要靠它）
	dc := poolEntryNamed("public-vpn-9")
	dc.IP = "198.51.100.7"
	if poolToNode(dc, "c").Residential {
		t.Error("public-vpn- 开头的是自营机房")
	}
}

// 超出上限时要先扔探测最旧的那些，而不是按国家或速度随机丢一批。
func TestFilterPoolServersKeepsFreshest(t *testing.T) {
	now := time.Now()
	list := []poolEntry{
		poolEntryNamed("vpn110", poolProbeAt(now.Add(-5*time.Hour))),
		poolEntryNamed("vpn111", poolProbeAt(now.Add(-1*time.Hour))),
		poolEntryNamed("vpn112", poolProbeAt(now.Add(-3*time.Hour))),
		poolEntryNamed("vpn113", poolStatus("unreachable")),
	}
	kept, nodes := filterPoolServers(list, "c", now)
	if len(nodes) != 3 || len(kept) != 3 {
		t.Fatalf("应有 3 个可用，实际 %d/%d", len(kept), len(nodes))
	}
	if nodes[0].HostName != "vpn111" || nodes[2].HostName != "vpn110" {
		t.Errorf("应按探测新→旧排，实际 %v", []string{nodes[0].HostName, nodes[1].HostName, nodes[2].HostName})
	}
}

func TestFilterPoolServersCaps(t *testing.T) {
	now := time.Now()
	list := make([]poolEntry, 0, poolMaxNodes+40)
	for i := 0; i < poolMaxNodes+40; i++ {
		// 间距用秒而不是分：超过 6 小时就被过滤掉，那样这个用例测的
		// 就不再是“截断”，而是“过滤”了（踩过）。
		list = append(list, poolEntryNamed(fmt.Sprintf("vpn%06d", i), poolProbeAt(now.Add(-time.Duration(i)*time.Second))))
	}
	_, nodes := filterPoolServers(list, "c", now)
	if len(nodes) != poolMaxNodes {
		t.Fatalf("应截断到 %d，实际 %d", poolMaxNodes, len(nodes))
	}
	// 被留下的应该是探测最新的那批：最新的 vpn000000 必须在，
	// 最旧的几个必须在外
	seen := map[string]bool{}
	for _, n := range nodes {
		seen[n.HostName] = true
	}
	if !seen["vpn000000"] {
		t.Error("探测最新的节点不该被截掉")
	}
	if seen[fmt.Sprintf("vpn%06d", poolMaxNodes+39)] {
		t.Error("探测最旧的节点应被截掉")
	}
}

// ---- 下载与校验 ----

// poolFixture 搭一个假的"GitHub + CDN"：一个 httptest 服务，
// 按路径区分索引、池文件、两份 config，外加一个 sha256 被人动过的副本。
type poolFixture struct {
	srv        *httptest.Server
	hitsCDN    atomic.Int32
	hitsRaw    atomic.Int32
	hitsIndex  atomic.Int32
	commit     string
	serversJS  string
	badSHAJSON string
}

func newPoolFixture(t *testing.T) *poolFixture {
	t.Helper()
	// 样本只有 3 个可用节点，把“至少 20 个”那道门槛降下来，
	// 不然整条路径跑不通（门槛本身的行为在下面的 TooFewUsable 用例里验）。
	prevMin := poolMinUsable
	poolMinUsable = 2
	t.Cleanup(func() { poolMinUsable = prevMin })

	f := &poolFixture{commit: strings.Repeat("3", 40)}

	entries := []poolEntry{
		poolEntryNamed("vpn100001"),
		poolEntryNamed("vpn100002"),
		poolEntryNamed("public-vpn-3"),
		poolEntryNamed("vpn100003", poolStatus("unreachable")),
	}
	srcFetched := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	serversBody, err := json.Marshal(poolServers{
		Kind: "vpngate-pool", SchemaVersion: 1, ServerCount: len(entries),
		SourceFetchedAt: srcFetched, Servers: entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.serversJS = string(serversBody)
	sum := sha256.Sum256(serversBody)
	indexBody, err := json.Marshal(map[string]any{
		"kind": "vpngate-pool", "schema_version": 1,
		"data_commit":       f.commit,
		"server_count":      len(entries),
		"source_fetched_at": srcFetched,
		"files": map[string]any{
			poolServersPath: map[string]any{
				"bytes":  len(serversBody),
				"sha256": hex.EncodeToString(sum[:]),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 一个节点的 .ovpn：要过 poolStore.config 里那两条合理性检查
	// （>=200 字节、含 remote），所以塞一段够长的假配置。
	ovpn := strings.Repeat("client\nremote 203.0.113.9 443\n", 20)
	cfgBody, err := json.Marshal(poolConfigFile{
		Kind:                "vpngate-pool-config",
		OpenVPNConfigBase64: base64.StdEncoding.EncodeToString([]byte(ovpn)),
		SHAPlain:            hashHex([]byte(ovpn)),
		OpenVPNConfigBytes:  len(ovpn),
	})
	if err != nil {
		t.Fatal(err)
	}
	badCfg, err := json.Marshal(poolConfigFile{
		Kind:                "vpngate-pool-config",
		OpenVPNConfigBase64: base64.StdEncoding.EncodeToString([]byte(ovpn + "被改过")),
		SHAPlain:            hashHex([]byte(ovpn)), // 内容变了但哈希没跟着变
		OpenVPNConfigBytes:  len(ovpn),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.badSHAJSON = string(badCfg)

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pool/latest.json":
			f.hitsIndex.Add(1)
			fmt.Fprint(w, string(indexBody))
		case "/cdn/pool/servers.json":
			f.hitsCDN.Add(1)
			fmt.Fprint(w, f.serversJS)
		case "/raw/pool/servers.json":
			f.hitsRaw.Add(1)
			fmt.Fprint(w, f.serversJS)
		case "/cdn/pool/configs/vpn100001.json", "/raw/pool/configs/vpn100001.json":
			fmt.Fprint(w, string(cfgBody))
		case "/cdn/pool/configs/vpnbadsha.json":
			fmt.Fprint(w, f.badSHAJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)

	// 下载地址是 var，就是为了让测试能指到这里（生产走 jsDelivr + Raw）
	prevURLs := poolFileURLs
	poolFileURLs = func(commit, path string) []string {
		return []string{f.srv.URL + "/cdn/" + path, f.srv.URL + "/raw/" + path}
	}
	t.Cleanup(func() { poolFileURLs = prevURLs })
	return f
}

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// poolIndexBody 拼一份与正文匹配的索引：sha256 和字节数都按正文算。
func poolIndexBody(commit, serversBody string) string {
	body, err := json.Marshal(map[string]any{
		"kind": "vpngate-pool", "schema_version": 1,
		"data_commit":       commit,
		"server_count":      4,
		"source_fetched_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		"files": map[string]any{
			poolServersPath: map[string]any{
				"bytes":  len(serversBody),
				"sha256": hashHex([]byte(serversBody)),
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func (f *poolFixture) store(t *testing.T) *poolStore {
	t.Helper()
	t.Setenv("FANOUT_VPNGATE_POOL", f.srv.URL+"/pool/latest.json")
	return newPoolStore(t.TempDir())
}

func TestPoolStoreGet(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)

	nodes, stat, err := s.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Err != "" || stat.Nodes != 3 {
		t.Errorf("统计不对: %+v", stat)
	}
	// 4 个条目里 vpn100003 是 unreachable，剩 3 个
	if len(nodes) != 3 {
		t.Fatalf("应有 3 个可用节点，实际 %d", len(nodes))
	}
	if s.commit != f.commit {
		t.Errorf("commit 没记下: %q", s.commit)
	}
	if s.sourceAt.IsZero() {
		t.Error("上游抓源站的时间应当接住，界面要显示新鲜度")
	}

	// 短时间内再问一次：不必再打网络
	f.hitsIndex.Store(0)
	if _, _, err := s.get(time.Now(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := f.hitsIndex.Load(); n != 0 {
		t.Errorf("poolRefreshInterval 内不该再拉索引，实际拉了 %d 次", n)
	}
}

// commit 没变时不能重拉那 7.1MB —— 这是"每 12 小时刷一次也不心疼带宽"的关键。
func TestPoolStoreSkipsDownloadWhenCommitUnchanged(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)
	if _, _, err := s.get(time.Now(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	beforeCDN, beforeRaw := f.hitsCDN.Load(), f.hitsRaw.Load()
	if beforeCDN+beforeRaw == 0 {
		t.Fatal("第一次应当真的拉过池文件")
	}
	// 越过 poolRefreshInterval，但 commit 没变
	s.fetched = time.Now().Add(-time.Hour)
	if _, _, err := s.get(time.Now(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := f.hitsCDN.Load() + f.hitsRaw.Load(); got != beforeCDN+beforeRaw {
		t.Errorf("commit 没变却重拉了池文件：%d → %d", beforeCDN+beforeRaw, got)
	}
	if f.hitsIndex.Load() < 2 {
		t.Error("索引还是要问一次的（只有 1KB）")
	}
}

func TestPoolStoreConfig(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)
	nodes, _, err := s.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string]Node{}
	for _, n := range nodes {
		byHost[n.HostName] = n
	}
	n, ok := byHost["vpn100001"]
	if !ok {
		t.Fatal("测试数据里没有 vpn100001")
	}
	if n.Config != "" {
		t.Fatal("列表里不该带 .ovpn")
	}
	cfg, err := s.config(n, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "remote 203.0.113.9 443") {
		t.Errorf("解码出来的配置不对: %q", cfg[:40])
	}
	// 第二次应当命中缓存，不再打网络
	before := f.hitsCDN.Load() + f.hitsRaw.Load()
	if _, err := s.config(n, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := f.hitsCDN.Load() + f.hitsRaw.Load(); got != before {
		t.Errorf("配置该命中缓存，实际又拉了一次（%d → %d）", before, got)
	}
}

// sha256 不符时必须报错，不能把坏内容当配置用下去。
func TestPoolStoreConfigRejectsBadSHA(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)
	n := Node{HostName: "vpnbadsha", RemoteConfig: &configRef{
		Commit: f.commit, Path: "pool/configs/vpnbadsha.json", SHA256: strings.Repeat("b", 64),
	}}
	if _, err := s.config(n, 5*time.Second); err == nil {
		t.Fatal("内容变了但哈希没变，应当校验失败")
	}
}

// CDN 挂了要能退到同 commit 的 GitHub Raw（两个地址指的是同一份内容）。
func TestPoolServersFallsBackToRaw(t *testing.T) {
	f := newPoolFixture(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn/pool/servers.json":
			http.Error(w, "cdn down", http.StatusInternalServerError)
		case "/raw/pool/servers.json":
			fmt.Fprint(w, f.serversJS)
		default:
			http.NotFound(w, r)
		}
	})
	idx := &poolIndex{
		Kind: "vpngate-pool", SchemaVersion: 1, DataCommit: f.commit, ServerCount: 4,
		Files: map[string]struct {
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		}{},
	}
	// 索引里的 sha256 必须与正文一致，否则两条路都会被校验挡掉
	var sum [32]byte = sha256.Sum256([]byte(f.serversJS))
	idx.Files[poolServersPath] = struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{Bytes: len(f.serversJS), SHA256: hex.EncodeToString(sum[:])}

	list, err := fetchPoolServers(idx, 5*time.Second)
	if err != nil {
		t.Fatalf("CDN 失败时应退到 raw: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("应拿到 4 条，实际 %d", len(list))
	}
}

// 索引里的 sha256 与正文不符 → 不能拿这份数据当列表用。
func TestPoolServersRejectsSHAConflict(t *testing.T) {
	f := newPoolFixture(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, f.serversJS)
	})
	idx := &poolIndex{DataCommit: f.commit, SchemaVersion: 1, ServerCount: 4, Files: map[string]struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{}}
	idx.Files[poolServersPath] = struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{Bytes: len(f.serversJS), SHA256: strings.Repeat("0", 64)}

	if _, err := fetchPoolServers(idx, 5*time.Second); err == nil {
		t.Fatal("sha256 不符应当报错")
	}
}

func TestFetchPoolIndexRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"schema 不认识", `{"kind":"vpngate-pool","schema_version":2,"data_commit":"` + strings.Repeat("3", 40) + `","server_count":100,"files":{}}`},
		{"kind 不对", `{"kind":"something-else","schema_version":1,"data_commit":"` + strings.Repeat("3", 40) + `","server_count":100,"files":{}}`},
		{"commit 太短", `{"kind":"vpngate-pool","schema_version":1,"data_commit":"abc","server_count":100,"files":{}}`},
		{"server_count 离谱", `{"kind":"vpngate-pool","schema_version":1,"data_commit":"` + strings.Repeat("3", 40) + `","server_count":90000,"files":{}}`},
		{"不是 JSON", `<!DOCTYPE html><html>被网关拦了</html>`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, c.body)
		}))
		_, err := fetchPoolIndex(srv.URL+"/latest.json", 5*time.Second)
		srv.Close()
		if err == nil {
			t.Errorf("%s: 应当报错", c.name)
		}
	}
}

// 上游改了格式（字段改名）时，池子会"安静地"变成 0 个可用节点。
// 那种列表不如不用：报错，让调用方沿用上一份。
func TestPoolStoreTooFewUsableKeepsOldList(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)
	nodes, _, err := s.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("前置条件不对: %d", len(nodes))
	}

	// 换成一个"格式对但一个能用的都没有"的池文件
	body := `{"servers":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pool/latest.json":
			idx, err := json.Marshal(map[string]any{
				"kind": "vpngate-pool", "schema_version": 1,
				"data_commit":  strings.Repeat("9", 40), // 换了 commit，绕过"没变化"这条快路
				"server_count": 4000,
				"files": map[string]any{
					poolServersPath: map[string]any{"bytes": len(body), "sha256": hashHex([]byte(body))},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(w, string(idx))
		default:
			fmt.Fprint(w, body)
		}
	}))
	defer srv.Close()
	t.Setenv("FANOUT_VPNGATE_POOL", srv.URL+"/pool/latest.json")
	s.fetched = time.Time{}

	kept, _, err := s.get(time.Now(), 5*time.Second)
	if err == nil {
		t.Fatal("可用节点过少应报错")
	}
	if len(kept) != 3 {
		t.Fatalf("报错时应返回上一份列表（%d 个），实际 %d", len(nodes), len(kept))
	}
	if _, err := json.Marshal(kept); err != nil {
		t.Error(err)
	}
}

// 关掉这个源（FANOUT_VPNGATE_POOL 设成空串）时必须安静地不参与统计，
// 也不能报错 —— 别的源还得照常工作。
func TestPoolStoreDisabled(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_POOL", "")
	s := newPoolStore(t.TempDir())
	nodes, stat, err := s.get(time.Now(), time.Second)
	if err != nil {
		t.Fatalf("关掉不该报错: %v", err)
	}
	if len(nodes) != 0 || stat.URL != "" {
		t.Errorf("关掉时不该产生统计项: %+v", stat)
	}
}

// 缓存要能跨重启：落盘的是过滤后的条目（带真实探测时间），
// 重启后先照用，再去索引确认有没有新版本。
func TestPoolCacheSurvivesRestart(t *testing.T) {
	f := newPoolFixture(t)
	dir := t.TempDir()
	t.Setenv("FANOUT_VPNGATE_POOL", f.srv.URL+"/pool/latest.json")

	s1 := newPoolStore(dir)
	nodes, _, err := s1.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("前置条件不对: %d", len(nodes))
	}

	f.hitsIndex.Store(0)
	s2 := newPoolStore(dir)
	got, stat, err := s2.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || stat.Nodes != 3 {
		t.Fatalf("重启后应直接从缓存拿到 3 个，实际 %d（%+v）", len(got), stat)
	}
	if f.hitsCDN.Load()+f.hitsRaw.Load() == 0 {
		t.Error("重启后拉了 7MB？缓存没起作用")
	}
	if s2.sourceAt.IsZero() {
		t.Error("上游时间应随缓存一起恢复，界面要显示")
	}
}

// RefreshNodes 必须把池源也放进统计里，界面上才看得出它在工作；
// 同时 CSV 优先：同名节点要沿用 CSV 的实时 Ping/Speed。
func TestRefreshNodesMergesPoolSource(t *testing.T) {
	f := newPoolFixture(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pool/latest.json":
			fmt.Fprint(w, poolIndexBody(f.commit, f.serversJS))
		case "/cdn/pool/servers.json":
			fmt.Fprint(w, f.serversJS)
		default:
			http.NotFound(w, r)
		}
	})
	t.Setenv("FANOUT_VPNGATE_POOL", f.srv.URL+"/pool/latest.json")
	t.Setenv("FANOUT_VPNGATE_SOURCES", "0")
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")

	// CSV 里放一个与池子重名的节点（vpn100001），速度标成 999 Mbps：
	// 合并后这个节点必须用 CSV 的值（实时），不能用池子的 50（快照）。
	cfgB64 := base64.StdEncoding.EncodeToString([]byte("client\nremote 1.2.3.4 443\n"))
	extra := "vpn100001,1.2.3.200,100,20,999000000,Japan,JP,3,1,1,1,2,op,," + cfgB64 + "\n"
	csv := strings.Replace(sampleNodeCSVN("m", 23), "*\n", extra+"*\n", 1)
	main := csvServer(t, csv)
	prevAPI := vpngateAPI
	vpngateAPI = main.URL
	t.Cleanup(func() { vpngateAPI = prevAPI })

	m := NewManager(20, t.TempDir())
	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatal(err)
	}
	stats := m.NodeSources()
	var poolSeen bool
	for _, s := range stats {
		if strings.Contains(s.URL, "pool/latest.json") {
			poolSeen = true
			if s.Err != "" || s.Nodes == 0 {
				t.Errorf("池源统计不对: %+v", s)
			}
		}
	}
	if !poolSeen {
		t.Errorf("统计里没有池源: %+v", stats)
	}
	poolN, at := m.PoolSummary()
	if poolN == 0 || at.IsZero() {
		t.Errorf("PoolSummary 应有内容: %d %v", poolN, at)
	}
	// 23 个 CSV + 3 个池内可用，重名的 vpn100001 只算一次
	if n != 26 {
		t.Errorf("合并后应 26 个，实际 %d", n)
	}
	for _, node := range m.nodes {
		if node.HostName == "vpn100001" {
			if node.SpeedMbps != 999 {
				t.Errorf("重名节点该用 CSV 的实时速度，实际 %v", node.SpeedMbps)
			}
			if node.RemoteConfig != nil {
				t.Error("CSV 的节点内联了 .ovpn，不该再带配置引用")
			}
			return
		}
	}
	t.Error("合并结果里没有 vpn100001")
}

// 第三方池的 .ovpn 引用要能一路走到"取配置"，否则挑中了也连不上。
func TestPoolNodesCarryConfigRefIntoTryNode(t *testing.T) {
	f := newPoolFixture(t)
	s := f.store(t)
	nodes, _, err := s.get(time.Now(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Config == "" && n.RemoteConfig == nil {
			t.Fatalf("%s 既没内联配置也没有引用", n.HostName)
		}
	}
}
