package main

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 站点页的真实结构（从 www.vpngate.net/en/sites.aspx 摘出的骨架）。
// 里面故意掺了几类干扰链接，用来验证它们不会被当成镜像：
//   - 站内导航（相对路径、无端口）
//   - 外站域名（softether.org）
//   - 回环与私有网段（万一页面被换过）
const mirrorSitesFixture = `<html><body>
<ul>
<li><a href="/en/sites.aspx" style="width: 280px">Mirror Sites List</a></li>
<li><a href="http://www.softether.org/5-download/src" style="width: 250px">Source Code of SoftEther VPN</a></li>
</ul>
<p>URL list of VPN Gate mirror sites (updated at
<span id="Label2">2026-11-05 09:18:53</span>&nbsp;UTC):</p>
<p>
<span id="Label1"><ul class='listBigArrow'>
<li><strong><span style='font-size: medium'><a href='http://150.40.105.16:33958/en/' target='_blank'>http://150.40.105.16:33958/en/</a></span></strong>&nbsp;(Mirror location: Croatia (LOCAL Name: Hrvatska))</li>
<li><strong><span style='font-size: medium'><a href='http://62.133.35.246:2265/en/' target='_blank'>http://62.133.35.246:2265/en/</a></span></strong>&nbsp;(Mirror location: Germany)</li>
<li><strong><span style='font-size: medium'><a href='http://150.40.105.6:11803/en/' target='_blank'>http://150.40.105.6:11803/en/</a></span></strong>&nbsp;(Mirror location: Croatia (LOCAL Name: Hrvatska))</li>
<li><strong><span style='font-size: medium'><a href='http://150.40.105.17:50406/en/' target='_blank'>http://150.40.105.17:50406/en/</a></span></strong>&nbsp;(Mirror location: Croatia)</li>
<li><strong><span style='font-size: medium'><a href='http://150.40.105.13:35090/en/' target='_blank'>http://150.40.105.13:35090/en/</a></span></strong>&nbsp;(Mirror location: Croatia (LOCAL Name: Hrvatska))</li>
<li><strong><span style='font-size: medium'><a href='http://150.40.105.12:29202/en/' target='_blank'>http://150.40.105.12:29202/en/</a></span></strong>&nbsp;(Mirror location: Croatia (LOCAL Name: Hrvatska))</li>
<li><strong><span style='font-size: medium'><a href='http://127.0.0.1:8080/en/'>loopback decoy</a></span></strong></li>
<li><strong><span style='font-size: medium'><a href='http://10.0.5.9:1194/en/'>private decoy</a></span></strong></li>
<li><strong><span style='font-size: medium'><a href='http://mirror.example.com:2265/en/'>domain decoy</a></span></strong></li>
</ul>
</span>
</p>
<ul class="listBigArrow">
<li style="font-size: medium"><strong><a href="https://www.vpngate.net/" target="_blank">https://www.vpngate.net/</a></strong></li>
</ul>
</body></html>`

func TestParseMirrorPageRealShape(t *testing.T) {
	got, err := parseMirrorPage(mirrorSitesFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"http://150.40.105.16:33958",
		"http://62.133.35.246:2265",
		"http://150.40.105.6:11803",
		"http://150.40.105.17:50406",
		"http://150.40.105.13:35090",
		"http://150.40.105.12:29202",
	}
	if len(got) != len(want) {
		t.Fatalf("镜像数不对: got %d (%v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Base != w {
			t.Errorf("第 %d 个镜像: got %q want %q", i, got[i].Base, w)
		}
	}
	// 回环、私有网段、域名、站内链接都不能被收进来
	for _, e := range got {
		if strings.Contains(e.Base, "example.com") || strings.Contains(e.Base, "127.0.0.1") || strings.Contains(e.Base, "10.0.5.9") {
			t.Fatalf("不该收进来的地址被收了: %q", e.Base)
		}
	}
	// location 要去掉本地名后缀，并且带上 /en/ 的根路径要归一化掉
	if got[0].Location != "Croatia" {
		t.Errorf("location 没收敛: %q", got[0].Location)
	}
	if got[1].Location != "Germany" {
		t.Errorf("location 没收敛: %q", got[1].Location)
	}
}

func TestParseMirrorPageFallsBackToWholePage(t *testing.T) {
	// 上游哪天改掉 Label1，全页扫描还得能捞出镜像
	body := `<html><body><div>
<a href='http://203.113.1.9:4433/cn/'>cn</a>
<a href="/en/about.aspx">nav</a>
</div></body></html>`
	got, err := parseMirrorPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Base != "http://203.113.1.9:4433" {
		t.Fatalf("全页兜底没捞到镜像: %+v", got)
	}
	if got[0].Location != "" {
		t.Errorf("兜底路径不该有 location: %q", got[0].Location)
	}
}

func TestParseMirrorPagePrefersLabel1Block(t *testing.T) {
	// Label1 那块能用时，块外的 IP:PORT 链接不得被收进来
	// （页脚、广告、以后新加的区块都不该变成源）。
	body := mirrorSitesFixture + "\n<ul><li><a href='http://192.95.9.9:655/en/'>outside</a></li></ul>"
	got, err := parseMirrorPage(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Base == "http://192.95.9.9:655" {
			t.Fatalf("Label1 块外的链接被收进来了: %+v", got)
		}
	}
	if len(got) != 6 {
		t.Fatalf("应当只有 6 个镜像: %+v", got)
	}
}

func TestParseMirrorPageNoEntriesIsError(t *testing.T) {
	if _, err := parseMirrorPage("<html><body><a href='/en/'>x</a></body></html>"); err == nil {
		t.Fatal("没有可用镜像时应当报错")
	}
}

func TestParseMirrorPageRespectsCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<span id='Label1'><ul>")
	for i := 0; i < maxMirrorEntries+6; i++ {
		sb.WriteString("<li><a href='http://192.95.2." + strconv.Itoa(i+1) + ":" + strconv.Itoa(3000+i) + "/en/'>x</a></li>")
	}
	sb.WriteString("</ul></span>")
	got, err := parseMirrorPage(sb.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxMirrorEntries {
		t.Fatalf("应封顶在 %d，实际 %d", maxMirrorEntries, len(got))
	}
}

func TestValidateMirrorBaseMatrix(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // 空串表示应当通过
		want    string
	}{
		{"正常", "http://150.40.105.16:33958/en/", "", "http://150.40.105.16:33958"},
		{"cn 后缀", "http://62.133.35.246:2265/cn/", "", "http://62.133.35.246:2265"},
		{"无后缀", "http://62.133.35.246:2265", "", "http://62.133.35.246:2265"},
		{"https", "https://203.113.1.7:8443/", "", "https://203.113.1.7:8443"},
		{"https 默认端口", "https://203.113.1.7", "", "https://203.113.1.7:443"},
		{"http 默认端口", "http://203.113.1.7", "", "http://203.113.1.7:80"},
		{"大写协议", "HTTP://203.113.1.7:8080/", "", "http://203.113.1.7:8080"},
		{"回环", "http://127.0.0.1:8080/en/", "回环", ""},
		{"私有 10/8", "http://10.0.5.9:1194/", "私有", ""},
		{"私有 192.168/16", "http://192.168.1.1:80/", "私有", ""},
		{"私有 172.16/12", "http://172.16.9.9:80/", "私有", ""},
		{"链路本地", "http://169.254.5.5:80/", "链路本地", ""},
		{"CGNAT", "http://100.64.1.2:80/", "保留段", ""},
		{"fake-IP 段", "http://198.18.1.58:80/", "保留段", ""},
		// 文档段在公网上不可路由，镜像不可能落在里面；
		// 真出现在列表页里就等于内容不对。
		{"TEST-NET-1", "http://192.0.2.9:80/", "保留段", ""},
		{"TEST-NET-2", "http://198.51.100.9:80/", "保留段", ""},
		{"TEST-NET-3", "http://203.0.113.9:80/", "保留段", ""},
		{"组播", "http://224.0.1.1:80/", "组播", ""},
		{"全零", "http://0.0.0.0:80/", "未指定", ""},
		{"域名", "http://mirror.example.com:2265/en/", "IPv4", ""},
		{"IPv6", "http://[2001:db8::1]:8080/", "IPv4", ""},
		{"协议不对", "ftp://203.0.113.7:21/", "协议", ""},
		{"端口越界", "http://203.113.1.7:70000/", "不合法", ""},
		{"端口非数字", "http://203.113.1.7:abc/", "无法解析", ""},
		{"缺主机", "http://:8080/", "主机", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, ip, port, err := validateMirrorBase(c.raw, nil)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应当通过: %v", err)
				}
				if base != c.want {
					t.Errorf("归一化结果: got %q want %q", base, c.want)
				}
				if ip == nil || port <= 0 {
					t.Errorf("应当解析出 IP 与端口: ip=%v port=%d", ip, port)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当拒绝 %q，结果 %q", c.raw, base)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误原因不明确: %v（期望包含 %q）", err, c.wantErr)
			}
		})
	}
}

func TestValidateMirrorBaseRejectsLocalListeningPort(t *testing.T) {
	local := func(p int) bool { return p == 8899 || p == 2265 }
	if _, _, _, err := validateMirrorBase("http://203.113.1.7:8899/", local); err == nil {
		t.Fatal("本机管理端口不该被当成镜像")
	}
	if _, _, _, err := validateMirrorBase("http://203.113.1.7:2265/", local); err == nil {
		t.Fatal("已分配的 SOCKS5 端口不该被当成镜像")
	}
	if _, _, _, err := validateMirrorBase("http://203.113.1.7:1194/", local); err != nil {
		t.Fatalf("不冲突的端口应当放行: %v", err)
	}
}

func TestMirrorSubnet(t *testing.T) {
	if got := mirrorSubnet("http://150.40.105.16:33958"); got != "150.40.105.0" {
		t.Fatalf("got %q", got)
	}
	if got := mirrorSubnet("http://62.133.35.246:2265"); got != "62.133.35.0" {
		t.Fatalf("got %q", got)
	}
	if got := mirrorSubnet("nonsense"); got != "" {
		t.Fatalf("坏输入应返回空，got %q", got)
	}
}

func TestPickMirrorSourcesDedupesSubnetAndLocation(t *testing.T) {
	entries, err := parseMirrorPage(mirrorSitesFixture)
	if err != nil {
		t.Fatal(err)
	}
	got := pickMirrorSources(entries, maxMirrorsPerFetch, 0)
	if len(got) != 2 {
		t.Fatalf("应当挑到 2 个: %+v", got)
	}
	if mirrorSubnet(got[0].Base) == mirrorSubnet(got[1].Base) {
		t.Fatalf("同一 /24 被挑了两次: %+v %+v", got[0], got[1])
	}
	if locationKey(got[0].Location) == locationKey(got[1].Location) {
		t.Fatalf("同一地区被挑了两次: %+v %+v", got[0], got[1])
	}
}

// 那 5 个克罗地亚镜像同网段同地区，所以可用源只有 2 个：
// 一个克罗地亚 + 德国。这正是按 location 去重的意义 ——
// 否则 6 个源里有 5 个在拉同一份内容。
func TestPickMirrorSourcesCapsAtDistinctSources(t *testing.T) {
	entries, err := parseMirrorPage(mirrorSitesFixture)
	if err != nil {
		t.Fatal(err)
	}
	got := pickMirrorSources(entries, 5, 0)
	if len(got) != 2 {
		t.Fatalf("去重后只剩 2 个不同源，实际 %+v", got)
	}
}

// 轮转必须真的换人：同一个 /24 里有 3 种不同快照，
// 跨轮次换着拉才能把并集持续做大。
func TestPickMirrorSourcesRotates(t *testing.T) {
	entries, err := parseMirrorPage(mirrorSitesFixture)
	if err != nil {
		t.Fatal(err)
	}
	first := pickMirrorSources(entries, 2, 0)
	second := pickMirrorSources(entries, 2, 2)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("两轮都该挑到 2 个: %v / %v", first, second)
	}
	if first[0].Base == second[0].Base {
		t.Fatalf("轮转没换掉首个源: %q", first[0].Base)
	}
	if mirrorSubnet(first[0].Base) != mirrorSubnet(second[0].Base) {
		t.Fatalf("克罗地亚那边应当还在同一个 /24: %q vs %q", first[0].Base, second[0].Base)
	}
	// 偏移超出列表长度要能绕回来，不能 panic 或返回空
	if got := pickMirrorSources(entries, 1, len(entries)*3+4); len(got) != 1 {
		t.Fatalf("大偏移应当照样挑得出: %+v", got)
	}
	if got := pickMirrorSources(entries, 2, -1); len(got) != 2 {
		t.Fatalf("负偏移应当挑得出: %+v", got)
	}
}

func TestPickMirrorSourcesEdgeCases(t *testing.T) {
	if got := pickMirrorSources(nil, 2, 0); got != nil {
		t.Fatalf("空列表应返回空: %+v", got)
	}
	if got := pickMirrorSources([]mirrorEntry{{Base: "http://203.0.113.1:1"}}, 0, 0); got != nil {
		t.Fatalf("n<=0 应返回空: %+v", got)
	}
	// 手改过的缓存里可能留下空 Base，不能被当成一个源
	if got := pickMirrorSources([]mirrorEntry{{Base: "  "}, {Base: "http://203.113.1.1:1"}}, 1, 0); len(got) != 1 || got[0].Base == "" {
		t.Fatalf("空 Base 应当跳过: %+v", got)
	}
}

func TestMirrorCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if entries, fetched, err := loadMirrors(dir); err != nil || entries != nil || !fetched.IsZero() {
		t.Fatalf("文件不存在应当是空结果而不是错误: %v %v %v", entries, fetched, err)
	}
	entries := []mirrorEntry{
		{Base: "http://150.40.105.16:33958", Location: "Croatia"},
		{Base: "http://62.133.35.246:2265", Location: "Germany"},
	}
	if err := saveMirrors(dir, entries); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mirrorsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("缓存权限应是 0600，实际 %v", info.Mode().Perm())
	}
	got, fetched, err := loadMirrors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Base != entries[1].Base {
		t.Fatalf("读回来的内容不对: %+v", got)
	}
	if fetched.IsZero() || time.Since(fetched) > time.Minute {
		t.Errorf("时间戳不新鲜: %v", fetched)
	}
}

func TestMirrorCacheCorruptIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(mirrorsPath(dir), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadMirrors(dir); err == nil {
		t.Fatal("坏缓存应当报错，让调用方去重拉而不是当成空列表")
	}
}

func TestMirrorListStale(t *testing.T) {
	if !mirrorListStale(time.Time{}, mirrorListMaxAge) {
		t.Error("没拉过应当算过期")
	}
	if mirrorListStale(time.Now().Add(-time.Hour), mirrorListMaxAge) {
		t.Error("1 小时前不该算过期")
	}
	if !mirrorListStale(time.Now().Add(-13*time.Hour), mirrorListMaxAge) {
		t.Error("13 小时前应当算过期")
	}
}

// sitesPageServer 把列表页 fixture 挂到本地服务上，并数被请求了几次。
func sitesPageServer(t *testing.T, body string, code int) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if code != http.StatusOK {
			http.Error(w, "nope", code)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestMirrorStore(t *testing.T) *mirrorStore {
	t.Helper()
	s := newMirrorStore(t.TempDir())
	// 不依赖跑测试那台机器的端口占用情况
	s.portBusy = func(int) bool { return false }
	return s
}

// 一轮 = 主站 + 按网段/地区去重后的镜像；列表页在一轮里只拉一次。
func TestMirrorStoreSourcesRefreshAndRotate(t *testing.T) {
	page, hits := sitesPageServer(t, mirrorSitesFixture, http.StatusOK)
	t.Setenv("FANOUT_VPNGATE_SITES", page.URL)
	s := newTestMirrorStore(t)

	first := s.sources(time.Now(), 5*time.Second)
	if len(first) != 1+maxMirrorsPerFetch {
		t.Fatalf("应是主站 + %d 个镜像: %v", maxMirrorsPerFetch, first)
	}
	if first[0] != vpngateAPI {
		t.Errorf("主站必须在第 0 位（它失败时要退自建反代）: %v", first)
	}
	// 6 个镜像去重后只剩两种：一个克罗地亚 + 德国
	joined := strings.Join(first, " ")
	if !strings.Contains(joined, "150.40.105.") || !strings.Contains(joined, "62.133.35.246") {
		t.Errorf("选出的源不对: %v", first)
	}
	for _, u := range first[1:] {
		if !strings.HasSuffix(u, "/api/iphone/") {
			t.Errorf("镜像要拼成节点列表接口: %q", u)
		}
	}
	if *hits != 1 {
		t.Errorf("列表页没过期不该重拉，实际请求 %d 次", *hits)
	}

	// 轮转：下一轮克罗地亚那边要换个 host
	second := s.sources(time.Now(), 5*time.Second)
	if strings.Join(second, " ") == strings.Join(first, " ") {
		t.Errorf("轮转没换源: %v", second)
	}
	if *hits != 1 {
		t.Errorf("重拉次数不对: %d", *hits)
	}

	// 缓存落盘：重启先读盘就靠这个
	got, fetched, err := loadMirrors(s.dir)
	if err != nil || len(got) != 6 || fetched.IsZero() {
		t.Fatalf("镜像列表没落盘: %d 条 %v err=%v", len(got), fetched, err)
	}
}

// 列表页拉不通：只用主站，并且按退避避开重试风暴。
func TestMirrorStoreBackoffAfterPageFailure(t *testing.T) {
	page, hits := sitesPageServer(t, "", http.StatusNotFound)
	t.Setenv("FANOUT_VPNGATE_SITES", page.URL)
	s := newTestMirrorStore(t)

	got := s.sources(time.Now(), 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("列表页挂了就只剩主站: %v", got)
	}
	if *hits != 1 {
		t.Fatalf("应当试过一次，实际 %d", *hits)
	}
	again := s.sources(time.Now(), 5*time.Second)
	if len(again) != 1 || *hits != 1 {
		t.Fatalf("退避期内不该再打列表页：次数=%d", *hits)
	}
}

// 列表页过期且重拉失败时，沿用旧缓存而不是退回单源。
func TestMirrorStoreKeepsCachedListWhenRefreshFails(t *testing.T) {
	page, _ := sitesPageServer(t, mirrorSitesFixture, http.StatusOK)
	t.Setenv("FANOUT_VPNGATE_SITES", page.URL)
	s := newTestMirrorStore(t)
	if got := s.sources(time.Now(), 5*time.Second); len(got) != 3 {
		t.Fatalf("第一轮应有 3 个源: %v", got)
	}
	// 让缓存过期，并把列表页换成拉不通
	s.fetched = time.Now().Add(-(mirrorListMaxAge + time.Hour))
	bad, _ := sitesPageServer(t, "", http.StatusServiceUnavailable)
	t.Setenv("FANOUT_VPNGATE_SITES", bad.URL)

	got := s.sources(time.Now(), 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("重拉失败要沿用旧镜像列表: %v", got)
	}
}

// 本机已在监听的端口不能被当成镜像（公网 IP + 自己的服务端口进得来）。
func TestMirrorStoreSkipsLocallyBusyPorts(t *testing.T) {
	page, _ := sitesPageServer(t, mirrorSitesFixture, http.StatusOK)
	t.Setenv("FANOUT_VPNGATE_SITES", page.URL)
	s := newTestMirrorStore(t)
	s.portBusy = func(p int) bool { return p == 33958 }

	got := s.sources(time.Now(), 5*time.Second)
	for _, u := range got {
		if strings.Contains(u, ":33958") {
			t.Fatalf("本机在用的端口不该被拉: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("克罗地亚那个被拦后应只剩主站 + 德国: %v", got)
	}

	s2 := newTestMirrorStore(t)
	s2.portBusy = func(int) bool { return true }
	if got := s2.sources(time.Now(), 5*time.Second); len(got) != 1 {
		t.Fatalf("所有镜像端口都被占用时应当只剩主站: %v", got)
	}
}

func TestMirrorsPerRoundEnv(t *testing.T) {
	if got := mirrorsPerRound(); got != maxMirrorsPerFetch {
		t.Errorf("默认应是 %d，实际 %d", maxMirrorsPerFetch, got)
	}
	t.Setenv("FANOUT_VPNGATE_SOURCES", "0")
	if got := mirrorsPerRound(); got != 0 {
		t.Errorf("设 0 应当关掉多源，实际 %d", got)
	}
	t.Setenv("FANOUT_VPNGATE_SOURCES", "abc")
	if got := mirrorsPerRound(); got != maxMirrorsPerFetch {
		t.Errorf("非法值应当退回默认，实际 %d", got)
	}

	page, _ := sitesPageServer(t, mirrorSitesFixture, http.StatusOK)
	t.Setenv("FANOUT_VPNGATE_SITES", page.URL)
	t.Setenv("FANOUT_VPNGATE_SOURCES", "0")
	s := newTestMirrorStore(t)
	if got := s.sources(time.Now(), 5*time.Second); len(got) != 1 {
		t.Fatalf("设 0 时只该有主站（不会去碰真镜像）: %v", got)
	}
}

func TestMirrorStoreSummaryAndNoteFailure(t *testing.T) {
	s := newTestMirrorStore(t)
	if n, at := s.summary(); n != 0 || !at.IsZero() {
		t.Errorf("初始应当是空: %d %v", n, at)
	}
	now := time.Now()
	s.noteFailure(now)
	if s.nextTry.Before(now) {
		t.Error("noteFailure 应当推后下次重试")
	}
	// 已经排得更晚时不能被拉近
	later := now.Add(2 * time.Hour)
	s.nextTry = later
	s.noteFailure(now)
	if !s.nextTry.Equal(later) {
		t.Errorf("不应把重试时间提前: %v", s.nextTry)
	}
}

func TestFeedURL(t *testing.T) {
	if got := feedURL("http://150.40.105.16:33958"); got != "http://150.40.105.16:33958/api/iphone/" {
		t.Fatalf("got %q", got)
	}
	if got := feedURL("http://150.40.105.16:33958/"); got != "http://150.40.105.16:33958/api/iphone/" {
		t.Fatalf("got %q", got)
	}
}

func TestSitesPageURLDisable(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_SITES", "")
	if got := sitesPageURL(); got != "" {
		t.Fatalf("显式设空应当关掉镜像列表页，got %q", got)
	}
	if _, err := fetchMirrorList(time.Second); err == nil {
		t.Fatal("关掉之后 fetchMirrorList 应当直接报错")
	}
	t.Setenv("FANOUT_VPNGATE_SITES", "https://example.invalid/sites.aspx")
	if got := sitesPageURL(); !strings.HasPrefix(got, "https://example.invalid") {
		t.Fatalf("got %q", got)
	}
}

// TestLiveMirrorProbe 打真实站点，把定源策略需要的指标一次测出来。
//
// 默认跳过 —— 它要出网，而且结果只反映跑它的那台机器的网络。
// 部署前在目标服务器上跑一次：
//
//	FANOUT_LIVE_TEST=1 go test -count=1 -run TestLiveMirrorProbe -v
//
// 可用 FANOUT_LIVE_TIMEOUT 调单源超时（默认 60s）：带宽差的机器上
// 1.3 MB 可能要更久（实测 46 KB/s 的链路下 20s 只拉到 917 KB）。
//
// 主站拉不通直接失败（没参考价值）；某些镜像拉不通逐项报错，
// 因为"哪些源在这台机器上可用"本身就是这个测试要输出的结论。
func TestLiveMirrorProbe(t *testing.T) {
	if os.Getenv("FANOUT_LIVE_TEST") == "" {
		t.Skip("设 FANOUT_LIVE_TEST=1 才跑（要打 VPN Gate 真实站点）")
	}
	timeout := 60 * time.Second
	if v := strings.TrimSpace(os.Getenv("FANOUT_LIVE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("FANOUT_LIVE_TIMEOUT 看不懂: %q", v)
		}
		timeout = d
	}

	entries, err := fetchMirrorList(30 * time.Second)
	if err != nil {
		t.Fatalf("镜像列表页拉不通: %v", err)
	}
	t.Logf("列表页给出 %d 个镜像：", len(entries))
	for _, e := range entries {
		t.Logf("  %-32s %-12s /24=%s", e.Base, e.Location, mirrorSubnet(e.Base))
	}

	// 缓存路径也得在目标机器上跑一次：重启先读盘就靠它
	dir := t.TempDir()
	if err := saveMirrors(dir, entries); err != nil {
		t.Fatalf("写镜像缓存失败: %v", err)
	}
	got, fetched, err := loadMirrors(dir)
	if err != nil || len(got) != len(entries) || fetched.IsZero() {
		t.Fatalf("读镜像缓存不对: %d 条 %v err=%v", len(got), fetched, err)
	}

	type src struct {
		label string
		url   string
		nodes []Node
		dur   time.Duration
		err   error
	}
	// 探测要看全貌，所以拉全部镜像，不受每轮 maxMirrorsPerFetch 限制
	all := []src{{label: "main", url: vpngateAPI}}
	for _, e := range entries {
		base, _, _, err := validateMirrorBase(e.Base, nil)
		if err != nil {
			t.Errorf("列表页给出的地址过不了校验，已跳过: %v", err)
			continue
		}
		all = append(all, src{label: strings.TrimPrefix(base, "http://"), url: feedURL(base)})
	}
	for i := range all {
		start := time.Now()
		nodes, err := fetchNodesFrom(all[i].url, "", timeout)
		all[i].dur, all[i].nodes, all[i].err = time.Since(start), nodes, err
	}

	t.Logf("单源超时 %s；各源结果：", timeout)
	t.Logf("  %-24s %6s %6s %6s %-10s %8s", "源", "节点", "家宽", "国家", "快照指纹", "用时")
	var ok []src
	for _, s := range all {
		if s.err != nil {
			t.Errorf("  %-24s 拉不通: %v (%s)", s.label, s.err, s.dur.Round(time.Second))
			continue
		}
		ok = append(ok, s)
		t.Logf("  %-24s %6d %6d %6d %-10s %7s",
			s.label, len(s.nodes), residentialCount(s.nodes), countryCount(s.nodes),
			snapshotFingerprint(s.nodes), s.dur.Round(time.Second))
	}
	if all[0].err != nil {
		t.Fatalf("主站拉不通，测不出东西: %v", all[0].err)
	}

	// 快照指纹相同 = 内容逐字节一致，同时当两个源用只是白花带宽
	byFp := map[string][]string{}
	for _, s := range ok {
		fp := snapshotFingerprint(s.nodes)
		byFp[fp] = append(byFp[fp], s.label)
	}
	for fp, labels := range byFp {
		if len(labels) > 1 {
			t.Logf("快照指纹 %s 重复：%s —— 按 location/网段去重是必要的", fp, strings.Join(labels, ", "))
		}
	}

	hosts := make([]map[string]bool, len(ok))
	for i, s := range ok {
		hosts[i] = hostnameSet(s.nodes)
	}
	t.Logf("两两交集（判读：10~20 值得轮转；60+ 固定 3 源就够）：")
	for i := 0; i < len(ok); i++ {
		for j := i + 1; j < len(ok); j++ {
			t.Logf("  %-24s ∩ %-24s = %d", ok[i].label, ok[j].label, intersect(hosts[i], hosts[j]))
		}
	}

	union := map[string]bool{}
	residential := map[string]bool{}
	regions := map[string]int{}
	for _, s := range ok {
		for _, n := range s.nodes {
			union[n.HostName] = true
			if n.Residential {
				residential[n.HostName] = true
				regions[n.CountryCode]++
			}
		}
	}
	t.Logf("合并后：总=%d 家宽=%d 国家=%d", len(union), len(residential), len(regions))
	t.Logf("家宽按地区：%s", topRegions(regions, 8))
	t.Logf("每轮取 主站 + %d 个不同 location 的镜像；跑完一轮换个 rotation", maxMirrorsPerFetch)
}

func hostnameSet(nodes []Node) map[string]bool {
	out := map[string]bool{}
	for _, n := range nodes {
		out[n.HostName] = true
	}
	return out
}

func intersect(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

func residentialCount(nodes []Node) int {
	n := 0
	for _, x := range nodes {
		if x.Residential {
			n++
		}
	}
	return n
}

func countryCount(nodes []Node) int {
	seen := map[string]bool{}
	for _, x := range nodes {
		if x.CountryCode != "" {
			seen[x.CountryCode] = true
		}
	}
	return len(seen)
}

// snapshotFingerprint 用 hostname 集合算一个短指纹。
// 不需要拿到原始响应体就能判断"两个源是不是同一份内容"。
func snapshotFingerprint(nodes []Node) string {
	names := make([]string, 0, len(nodes))
	for _, x := range nodes {
		names = append(names, x.HostName)
	}
	sort.Strings(names)
	sum := md5.Sum([]byte(strings.Join(names, "\n")))
	return fmt.Sprintf("%x", sum[:4])
}

func topRegions(counts map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	if len(list) > n {
		list = list[:n]
	}
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, fmt.Sprintf("%s=%d", x.k, x.v))
	}
	return strings.Join(parts, " ")
}
