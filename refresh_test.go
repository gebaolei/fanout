package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useMainFeed 把主站地址指向测试服务。vpngateAPI 是 var 就是为了这个，
// 跟 fetchNodesWith 把直连地址拆成参数是同一个思路。
func useMainFeed(t *testing.T, url string) {
	t.Helper()
	old := vpngateAPI
	vpngateAPI = url
	t.Cleanup(func() { vpngateAPI = old })
}

// noAgg 关掉两个额外源：自建反代兜底与聚合中心。
//
// 这些测试只关心官方 CSV 这条路径本身。不关聚合中心的话它会真去连
// FANOUT_AGG_API（慢，而且会把“全部源失败”的用例变成“有货”）；
// 聚合中心自己的行为在 aggsource_test.go 里覆盖，合并行为在下面覆盖。
func noAgg(t *testing.T) {
	t.Helper()
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	t.Setenv("FANOUT_AGG_API", "")
}

// 全部源失败时必须保留上一轮列表。这个语义改动前就有，多源后不能丢。
func TestRefreshNodesKeepsPreviousListWhenAllFail(t *testing.T) {
	noAgg(t)
	useMainFeed(t, statusServer(t, 403).URL)

	m := NewManager(20, t.TempDir())
	m.nodes = []Node{{HostName: "keep-me", CountryCode: "JP", Residential: true}}
	m.fetched = time.Now().Add(-time.Hour)

	if _, err := m.RefreshNodes(); err == nil {
		t.Fatal("全部源失败应当报错")
	}
	if len(m.nodes) != 1 || m.nodes[0].HostName != "keep-me" {
		t.Fatalf("列表被清空了: %v", names(m.nodes))
	}
	if m.fetched.IsZero() {
		t.Error("刷新失败不该改动 fetched 时间")
	}
	// 界面要能看出是"三个源都拉不动"还是"只有主站挂了"
	stats := m.NodeSources()
	if len(stats) == 0 || stats[0].Err == "" {
		t.Errorf("失败也要记下各源结果: %+v", stats)
	}
}

func TestRefreshNodesSuccessRecordsStats(t *testing.T) {
	noAgg(t)
	main := csvServer(t, sampleNodeCSVN("m", 24))
	useMainFeed(t, main.URL)

	m := NewManager(20, t.TempDir())
	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 24 {
		t.Fatalf("应当是 24 个，实际 %d", n)
	}
	if m.fetched.IsZero() {
		t.Error("fetched 应当更新")
	}
	stats := m.NodeSources()
	if len(stats) != 1 || stats[0].Err != "" || stats[0].Nodes != 24 {
		t.Errorf("各源结果不对: %+v", stats)
	}
}

// 启动路径用的单源刷新：不碰多源，也不写 stats（那一轮还没有多源数据）。
func TestRefreshMainNodesIsSingleSource(t *testing.T) {
	noAgg(t)
	main := csvServer(t, sampleNodeCSVN("m", 20))
	useMainFeed(t, main.URL)

	m := NewManager(20, t.TempDir())
	n, err := m.RefreshMainNodes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("got %d", n)
	}
	if len(m.NodeSources()) != 0 {
		t.Errorf("单源刷新不该写多源统计: %+v", m.NodeSources())
	}

	// 主站挂了、反代也关掉 → 报错，并且不动已有列表
	useMainFeed(t, statusServer(t, 403).URL)
	if _, err := m.RefreshMainNodes(); err == nil {
		t.Fatal("主站挂且反代关掉应当报错")
	}
	if len(m.nodes) != 20 {
		t.Fatalf("失败不该清空列表: %d", len(m.nodes))
	}
}

// 聚合中心的节点要跟官方主站的合并，同名节点沿用主站那一份。
//
// 主站是实时生成的，agg 的池子是它自己那台机器某一轮拉下的快照：实测同一个
// 节点在两处的 Ping/Speed 会不一致，且 agg 那份是旧值。
func TestRefreshNodesMergesAggSource(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	main := csvServer(t, sampleNodeCSVN("m", 30))
	useMainFeed(t, main.URL)

	f := newAggFixture(t, 40)
	f.wantAuth = "Bearer " + f.token
	// 两个与主站同名的节点，但带的是旧值：合并时必须沿用主站那份
	oldCfg := base64.StdEncoding.EncodeToString([]byte("client\nremote 9.9.9.9 443\n"))
	for i := 0; i < 2; i++ {
		f.rows[i].HostName = fmt.Sprintf("m%d", i+1)
		f.rows[i].Ping = 999
		f.rows[i].OpenVPNConfigB64 = oldCfg
	}

	m := NewManager(20, t.TempDir())
	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 30+40-2 {
		t.Errorf("去重后应当是 68 个，实际 %d", n)
	}
	for _, node := range m.nodes {
		if node.HostName == "m1" || node.HostName == "m2" {
			if node.Ping != 20 {
				t.Errorf("同名节点应当沿用主站那份（Ping 20），实际 %d", node.Ping)
			}
		}
	}
	stats := m.NodeSources()
	if len(stats) != 2 {
		t.Fatalf("主站与聚合中心各一条统计，实际 %+v", stats)
	}
}

// 聚合中心挂了不影响这一轮：主站有货就算成功，但统计里要留下失败原因。
// 不写这一行，界面上就只能看到“源 1/2”却不知道另一个为什么挂。
func TestRefreshNodesSurvivesAggFailure(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	main := csvServer(t, sampleNodeCSVN("m", 30))
	useMainFeed(t, main.URL)

	f := newAggFixture(t, 30)
	f.status = http.StatusTooManyRequests

	m := NewManager(20, t.TempDir())
	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatalf("聚合中心挂了不该让整轮失败: %v", err)
	}
	if n != 30 {
		t.Errorf("应当只拿到主站的 30 个，实际 %d", n)
	}
	stats := m.NodeSources()
	var agg *sourceStat
	for i, s := range stats {
		if strings.Contains(s.URL, "vpngate/api/iphone/") && !strings.Contains(s.URL, "www.vpngate.net") {
			agg = &stats[i]
		}
	}
	if agg == nil {
		t.Fatalf("统计里应当有聚合中心那一条，哪怕它挂了: %+v", stats)
	}
	if !strings.Contains(agg.Err, "429") {
		t.Errorf("那一条要记下失败原因，实际 %q", agg.Err)
	}
}

// 官方源全部失败（主站限流时真会这样）时，聚合中心要能顶上。
// 列表会比官方名单旧一点（agg 自己 60 分钟一轮），但比空列表强。
func TestRefreshNodesFallsBackToAggWhenMainFails(t *testing.T) {
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
	useMainFeed(t, statusServer(t, 403).URL)

	f := newAggFixture(t, 35)
	f.wantAuth = "Bearer " + f.token

	m := NewManager(20, t.TempDir())
	m.nodes = []Node{{HostName: "keep-me", CountryCode: "JP", Residential: true}}

	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatalf("官方源挂了、聚合中心有货，不该报错: %v", err)
	}
	if n != 35 {
		t.Errorf("应当拿聚合中心的 35 个顶上，实际 %d", n)
	}
	if has(m.nodes, "keep-me") {
		t.Error("有更新的列表可用时不该继续沿用旧列表")
	}
}

// 启动后的后台聚合、12 小时 ticker、重连退避前的刷新、手动接口可能撞在一起。
// 开 -race 同跑几轮，确认不会互相踩到写出半份列表。
func TestConcurrentRefreshDoesNotTearTheList(t *testing.T) {
	noAgg(t)
	main := csvServer(t, sampleNodeCSVN("m", 30))
	useMainFeed(t, main.URL)

	m := NewManager(20, t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.RefreshNodes(); err != nil {
				t.Errorf("刷新失败: %v", err)
			}
			got, _ := m.Nodes()
			if len(got) != 30 {
				t.Errorf("读到的列表不完整: %d", len(got))
			}
		}()
	}
	wg.Wait()
	if len(m.nodes) != 30 {
		t.Fatalf("最终列表应当是 30 个，实际 %d", len(m.nodes))
	}
}

// 重连循环里的刷新必须受冷却控制：一条连不上的隧道每轮重试都刷一轮的话，
// 一小时上百次请求会把主站和自建反代打进限流，真需要刷新时反而刷不动。
func TestRetryRefreshHonoursCooldown(t *testing.T) {
	noAgg(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sampleNodeCSVN("m", 30))
	}))
	defer srv.Close()
	useMainFeed(t, srv.URL)

	m := NewManager(20, t.TempDir())
	for i := 0; i < 3; i++ {
		m.retryRefresh(10 * time.Minute)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("冷却内只该拉一次，实际 %d 次", n)
	}

	m.mu.Lock()
	m.lastTry = time.Now().Add(-11 * time.Minute)
	m.mu.Unlock()
	m.retryRefresh(10 * time.Minute)
	if n := hits.Load(); n != 2 {
		t.Fatalf("冷却过了应当再拉一次，实际 %d 次", n)
	}
}

// 全部源失败时 m.fetched 不会更新，冷却必须记在“发起过拉取”上。
// 否则主站被限流的那段时间会被越打越凶 —— 正是现场看到的那个问题。
func TestRetryRefreshCooldownSurvivesTotalFailure(t *testing.T) {
	noAgg(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	useMainFeed(t, srv.URL)

	m := NewManager(20, t.TempDir())
	for i := 0; i < 4; i++ {
		m.retryRefresh(10 * time.Minute)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("全失败时也只该拉一次，实际 %d 次", n)
	}
	if !m.fetched.IsZero() {
		t.Fatal("失败不该更新 fetched（所以冷却靠 lastTry）")
	}
}

// TestLiveRefreshNodes 把生产刷新路径（选源 + 并发拉 + 合并 + 数量检查）跑在真站上。
// 默认跳过；部署前在目标服务器上跑：
//
//	FANOUT_LIVE_TEST=1 FANOUT_VPNGATE_TIMEOUT=60s go test -run TestLiveRefreshNodes -v
//
// 个别源不通不算失败（那正是要在目标机器上看的），但合并结果必须过
// 最低节点数，否则等于聚合白做。
func TestLiveRefreshNodes(t *testing.T) {
	if os.Getenv("FANOUT_LIVE_TEST") == "" {
		t.Skip("设 FANOUT_LIVE_TEST=1 才跑（要打 VPN Gate 真实站点）")
	}
	m := NewManager(20, t.TempDir())
	n, err := m.RefreshNodes()
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	for _, s := range m.NodeSources() {
		if s.Err != "" {
			t.Logf("  源 %s 失败：%s（%dms）", s.URL, s.Err, s.MS)
			continue
		}
		t.Logf("  源 %s OK：%d 个节点（%dms）", s.URL, s.Nodes, s.MS)
	}
	t.Logf("合并后 %d 个节点", n)
	if n < minNodesPerSource {
		t.Errorf("合并后只有 %d 个节点，不对", n)
	}
	regions := map[string]int{}
	for _, node := range m.nodes {
		if node.Residential {
			regions[node.CountryCode]++
		}
	}
	t.Logf("家宽覆盖 %d 个国家", len(regions))
}
