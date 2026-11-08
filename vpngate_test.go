package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func sampleNodeCSV(host string) string {
	cfg := base64.StdEncoding.EncodeToString([]byte("client\nremote 1.2.3.4 443\n"))
	return "*vpn_servers\n" +
		"#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64\n" +
		fmt.Sprintf("%s,1.2.3.4,100,20,10000000,Japan,JP,3,1,1,1,2,op,,%s\n", host, cfg) +
		"*\n"
}

// 直连能用时不该碰反代。
func TestFetchNodesUsesDirectWhenHealthy(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNodeCSV("direct-node"))
	}))
	defer direct.Close()
	mirrorHit := false
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHit = true
		fmt.Fprint(w, sampleNodeCSV("mirror-node"))
	}))
	defer mirror.Close()

	t.Setenv("FANOUT_VPNGATE_MIRROR", mirror.URL)
	nodes, err := fetchNodesWith(direct.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("直连应该成功: %v", err)
	}
	if nodes[0].HostName != "direct-node" {
		t.Fatalf("拿到的不是直连结果: %s", nodes[0].HostName)
	}
	if mirrorHit {
		t.Fatal("直连成功时不应该请求反代")
	}
}

// 直连被拦截时回落到反代，并带上访问密钥。
func TestFetchNodesFallsBackToMirror(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer direct.Close()
	gotKey := ""
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Fanout-Key")
		fmt.Fprint(w, sampleNodeCSV("mirror-node"))
	}))
	defer mirror.Close()

	t.Setenv("FANOUT_VPNGATE_MIRROR", mirror.URL)
	t.Setenv("FANOUT_VPNGATE_MIRROR_KEY", "test-key")
	nodes, err := fetchNodesWith(direct.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("反代应该兜住: %v", err)
	}
	if nodes[0].HostName != "mirror-node" {
		t.Fatalf("没走反代: %s", nodes[0].HostName)
	}
	if gotKey != "test-key" {
		t.Fatalf("反代请求没带密钥: %q", gotKey)
	}
}

// 直连返回 200 但内容是被劫持的门户页，也要回落。
func TestFetchNodesFallsBackOnGarbageBody(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>请先登录网络</body></html>")
	}))
	defer direct.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNodeCSV("mirror-node"))
	}))
	defer mirror.Close()

	t.Setenv("FANOUT_VPNGATE_MIRROR", mirror.URL)
	nodes, err := fetchNodesWith(direct.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("内容异常时应该回落: %v", err)
	}
	if nodes[0].HostName != "mirror-node" {
		t.Fatalf("没走反代: %s", nodes[0].HostName)
	}
}

// 反代被显式关掉时，直连失败就直接报错。
func TestFetchNodesMirrorDisabled(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer direct.Close()

	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	if _, err := fetchNodesWith(direct.URL, 5*time.Second); err == nil {
		t.Fatal("关掉反代后应该直接失败")
	}
}

// sampleNodeCSVN 造一个含 n 个节点的完整 CSV。
// 多源的测试需要每个源都过“数量合理性检查”（实测单站 97~101 个，
// 少于 minNodesPerSource 会被当异常丢弃），所以造数据得造够。
func sampleNodeCSVN(prefix string, n int) string {
	cfg := base64.StdEncoding.EncodeToString([]byte("client\nremote 1.2.3.4 443\n"))
	var b strings.Builder
	b.WriteString("*vpn_servers\n")
	b.WriteString("#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64\n")
	for i := 0; i < n; i++ {
		// 速度按序号递减，合并后的排序结果可以直接算
		fmt.Fprintf(&b, "%s%d,1.2.3.%d,100,20,%d,Japan,JP,3,1,1,1,2,op,,%s\n",
			prefix, i, i%250, (n-i)*1000000, cfg)
	}
	b.WriteString("*\n")
	return b.String()
}

func csvServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func names(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.HostName)
	}
	return out
}

func has(nodes []Node, host string) bool {
	for _, n := range nodes {
		if n.HostName == host {
			return true
		}
	}
	return false
}

func nodeAt(nodes []Node, host string) Node {
	for _, n := range nodes {
		if n.HostName == host {
			return n
		}
	}
	return Node{}
}

// 两边都有的节点要用主站（靠前的源）的元数据：镜像是缓存快照，
// 实测 37/291 个节点的 Score/Ping/Speed 在镜像里是旧值。
func TestMergeNodeSourcesPrefersEarlierSource(t *testing.T) {
	main := []Node{
		{HostName: "a", IP: "1.1.1.1", SpeedMbps: 10, Ping: 5},
		{HostName: "b", IP: "2.2.2.2", SpeedMbps: 1, Ping: 7},
	}
	mirror := []Node{
		{HostName: "b", IP: "2.2.2.2", SpeedMbps: 900, Ping: 999},
		{HostName: "c", IP: "3.3.3.3", SpeedMbps: 500, Ping: 1},
	}
	got := mergeNodeSources([][]Node{main, mirror})
	if len(got) != 3 {
		t.Fatalf("去重后应当只剩 3 个: %v", names(got))
	}
	if b := nodeAt(got, "b"); b.SpeedMbps != 1 || b.Ping != 7 {
		t.Errorf("b 的元数据应当沿用靠前的源，实际 %+v", b)
	}
	// 合并后要按速度重排：c(500) > a(10) > b(1)
	if got[0].HostName != "c" || got[1].HostName != "a" || got[2].HostName != "b" {
		t.Errorf("排序不对: %v", names(got))
	}
}

// 同名不同 IP 上游不该出现（实测 7 个源 0 次），真撞上时沿用靠前的，不去猜。
func TestMergeNodeSourcesKeepsFirstOnIPConflict(t *testing.T) {
	got := mergeNodeSources([][]Node{
		{{HostName: "a", IP: "1.1.1.1", SpeedMbps: 1}},
		{{HostName: "a", IP: "9.9.9.9", SpeedMbps: 500}},
	})
	if len(got) != 1 || got[0].IP != "1.1.1.1" {
		t.Fatalf("应当保留靠前的那条: %+v", got)
	}
}

func TestMergeNodeSourcesEmpty(t *testing.T) {
	if got := mergeNodeSources(nil); len(got) != 0 {
		t.Fatalf("无源应返回空: %+v", got)
	}
}

func TestNodeCountPlausible(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		prev  int
		isErr bool
	}{
		{"正常", 97, 100, false},
		{"首轮无基准", 25, 0, false},
		{"过少", 3, 0, true},
		{"刚好卡线", minNodesPerSource, 0, false},
		{"比上一轮少一半以上", 40, 100, true},
		{"刚好一半", 50, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := nodeCountPlausible(c.n, c.prev)
			if c.isErr && err == nil {
				t.Fatalf("n=%d prev=%d 应当拒", c.n, c.prev)
			}
			if !c.isErr && err != nil {
				t.Fatalf("n=%d prev=%d 应当过: %v", c.n, c.prev, err)
			}
		})
	}
}

// 一个源挂了不影响整体，成功的源合并起来。
func TestFetchAggregatedMergesAndToleratesPartialFailure(t *testing.T) {
	main := csvServer(t, sampleNodeCSVN("m", 25))
	mirror := csvServer(t, sampleNodeCSVN("n", 25))
	bad := statusServer(t, http.StatusServiceUnavailable)

	got, stats, err := fetchAggregated([]string{main.URL, mirror.URL, bad.URL}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("部分源失败不该整体失败: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("合并后应有 50 个，实际 %d", len(got))
	}
	if !has(got, "m0") || !has(got, "n0") {
		t.Errorf("两边的节点都要在: %v", names(got))
	}
	if len(stats) != 3 || stats[0].Err != "" || stats[1].Err != "" || stats[2].Err == "" {
		t.Errorf("各源结果不对: %+v", stats)
	}
	if stats[0].Nodes != 25 || stats[1].Nodes != 25 {
		t.Errorf("计数不对: %+v", stats)
	}
	if stats[0].MS < 0 {
		t.Errorf("耗时应为非负: %+v", stats[0])
	}
}

// 重叠部分只留一份。
func TestFetchAggregatedDedupesAcrossSources(t *testing.T) {
	main := csvServer(t, sampleNodeCSVN("x", 20))
	mirror := csvServer(t, sampleNodeCSVN("x", 20))
	got, _, err := fetchAggregated([]string{main.URL, mirror.URL}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("完全重叠应当还是 20 个，实际 %d", len(got))
	}
}

// 语法完整但只有几个节点的响应：parseNodeCSV 不会报错，
// 必须靠数量检查丢掉，否则池子会静默缩到零头。
func TestFetchAggregatedDropsShortButCompleteSource(t *testing.T) {
	main := csvServer(t, sampleNodeCSVN("m", 30))
	short := csvServer(t, sampleNodeCSVN("s", 3))
	got, stats, err := fetchAggregated([]string{main.URL, short.URL}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 30 || has(got, "s0") {
		t.Fatalf("残缺源应当被丢，实际 %d 个: %v", len(got), names(got))
	}
	if stats[1].Err == "" || !strings.Contains(stats[1].Err, "过少") {
		t.Errorf("应当记“过少”: %+v", stats[1])
	}
}

// 相比上一轮骤减也算异常（上游只返了半截但完整交付）。
// 基准是合并后的总数，所以拿上一轮 60 做例：40 个的源正常，20 个的源算骤减。
func TestFetchAggregatedDropsSourceThatCollapsed(t *testing.T) {
	main := csvServer(t, sampleNodeCSVN("m", 40))
	half := csvServer(t, sampleNodeCSVN("h", 20))
	got, stats, err := fetchAggregated([]string{main.URL, half.URL}, 5*time.Second, map[string]int{half.URL: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 40 || has(got, "h0") {
		t.Fatalf("骤减的源应当被丢，实际 %d", len(got))
	}
	if stats[1].Err == "" || !strings.Contains(stats[1].Err, "上一轮") {
		t.Errorf("应当记骤减: %+v", stats[1])
	}
}

func TestFetchAggregatedAllSourcesFail(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	_, stats, err := fetchAggregated([]string{statusServer(t, 403).URL, statusServer(t, 404).URL}, 5*time.Second, nil)
	if err == nil {
		t.Fatal("全部失败应当报错")
	}
	if len(stats) != 2 || stats[0].Err == "" || stats[1].Err == "" {
		t.Errorf("各源失败原因都要记下来: %+v", stats)
	}
}

func TestFetchAggregatedNoSources(t *testing.T) {
	if _, _, err := fetchAggregated(nil, time.Second, nil); err == nil {
		t.Fatal("没有源应当报错")
	}
}

// 主站挂了才去退自建反代；镜像挂了不会退。
func TestFetchAggregatedWorkerFallbackIsMainOnly(t *testing.T) {
	worker := csvServer(t, sampleNodeCSVN("w", 22))
	t.Setenv("FANOUT_VPNGATE_MIRROR", worker.URL)
	main := statusServer(t, http.StatusForbidden)
	mirror := statusServer(t, http.StatusBadGateway)

	got, stats, err := fetchAggregated([]string{main.URL, mirror.URL}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("主站失败应当由反代兜住: %v", err)
	}
	if len(got) != 22 || !has(got, "w0") {
		t.Fatalf("拿到的不是反代结果: %d 个", len(got))
	}
	if stats[0].Err != "" {
		t.Errorf("反代成功时主站不应记错: %+v", stats[0])
	}
	if stats[1].Err == "" {
		t.Errorf("镜像失败要记下来: %+v", stats[1])
	}
}

// 超限的响应必须丢，不能把半截列表当成完整列表。
func TestParseNodeCSVRejectsHTML(t *testing.T) {
	// 限流和拦截页也是 HTTP 200，但正文是 HTML。报错要能看出是这回事，
	// 而不是「bare " in non-quoted-field」这种得倒推的提法。
	body := "<!DOCTYPE html><html lang=\"zh-CN\"><head><title>Too Many Requests</title></head></html>\n"
	_, err := parseNodeCSV(body)
	if err == nil {
		t.Fatal("HTML 应当报错")
	}
	if !strings.Contains(err.Error(), "不是节点列表") {
		t.Fatalf("报错应当说明返回的不是节点列表: %v", err)
	}
	if !strings.Contains(err.Error(), "<!DOCTYPE html>") {
		t.Fatalf("报错里该带上首行预览: %v", err)
	}
}

func TestParseNodeCSVAcceptsLeadingBlankLines(t *testing.T) {
	body := "\r\n\r\n" + sampleNodeCSVN("a", 24)
	nodes, err := parseNodeCSV(body)
	if err != nil {
		t.Fatalf("开头的空行不该影响解析: %v", err)
	}
	if len(nodes) != 24 {
		t.Fatalf("应当解析出 24 个，实际 %d", len(nodes))
	}
}

func TestFetchNodesFromDoesNotAskForGzip(t *testing.T) {
	// 官方镜像站对带 Accept-Encoding: gzip 的请求会先回 200 再一个字节都不发，
	// 挂到客户端超时。Go 的默认 Transport 会自动加这个头，不关掉就等于
	// 所有镜像源永远失败（跳 curl 不带这个头则 1.3 MB 几秒拉完）。
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept-Encoding")
		fmt.Fprint(w, sampleNodeCSVN("a", 24))
	}))
	defer srv.Close()

	if _, err := fetchNodesFrom(srv.URL, "", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("不该请求压缩编码，实际发了 Accept-Encoding: %q", got)
	}
}

func TestFetchNodesFromRejectsOversizeBody(t *testing.T) {
	big := csvServer(t, strings.Repeat("x", csvMaxBytes+1))
	if _, err := fetchNodesFrom(big.URL, "", 5*time.Second); err == nil {
		t.Fatal("超过上限的响应应当报错")
	} else if !strings.Contains(err.Error(), "超过") {
		t.Errorf("错误原因不明: %v", err)
	}
}

func TestNodeFetchTimeout(t *testing.T) {
	if got := nodeFetchTimeout(); got != 25*time.Second {
		t.Errorf("默认应是 25s，实际 %v", got)
	}
	t.Setenv("FANOUT_VPNGATE_TIMEOUT", "90s")
	if got := nodeFetchTimeout(); got != 90*time.Second {
		t.Errorf("应当能被环境变量覆盖，实际 %v", got)
	}
	t.Setenv("FANOUT_VPNGATE_TIMEOUT", "0")
	if got := nodeFetchTimeout(); got != 25*time.Second {
		t.Errorf("非法值应当退回默认，实际 %v", got)
	}
}

// TestLiveNodeConfigCoverage 数一下源站列表里有多少节点其实没带 .ovpn。
//
// 真机上撞到过这么一行：
//
//	出口 4 候选 vpn200460917（）连不上: 节点 vpn200460917 没有 .ovpn 配置
//
// 这种节点永远连不上，却照样占掉一次候选名额（一次要 40 多秒）和 6 分钟
// 黑名单。先量清楚比例再决定在哪一层挡掉。只在 FANOUT_LIVE_TEST=1 时跑。
func TestLiveNodeConfigCoverage(t *testing.T) {
	if os.Getenv("FANOUT_LIVE_TEST") != "1" {
		t.Skip("设 FANOUT_LIVE_TEST=1 才打真站")
	}
	nodes, err := fetchNodes(nodeFetchTimeout())
	if err != nil {
		t.Fatal(err)
	}
	none, noSession := 0, 0
	for _, n := range nodes {
		if n.Config == "" && n.RemoteConfig == nil {
			none++
		}
		if n.Sessions == 0 {
			noSession++
		}
	}
	t.Logf("源站列表 %d 个节点：没带 .ovpn 的 %d 个，sessions=0 的 %d 个", len(nodes), none, noSession)
	for _, n := range nodes {
		if n.Config == "" && n.RemoteConfig == nil {
			t.Logf("  没配置的节点: %s %s %s 速度 %v Mbps", n.HostName, n.IP, n.CountryCode, n.SpeedMbps)
		}
	}
}
