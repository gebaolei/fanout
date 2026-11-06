package main

import (
	"os"
	"sync"
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

// noMirrors 关掉多源选源。这些测试不想去碰真镜像的地址 ——
// 选源与去重的行为在 mirrors_test.go 里覆盖，合并行为在 vpngate_test.go 里。
func noMirrors(t *testing.T) {
	t.Helper()
	t.Setenv("FANOUT_VPNGATE_SOURCES", "0")
	t.Setenv("FANOUT_VPNGATE_MIRROR", "")
}

// 全部源失败时必须保留上一轮列表。这个语义改动前就有，多源后不能丢。
func TestRefreshNodesKeepsPreviousListWhenAllFail(t *testing.T) {
	noMirrors(t)
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
	noMirrors(t)
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
	noMirrors(t)
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

// NewManager 会读 workDir/mirrors.json，重启后不必等网络就有一版镜像列表。
func TestNewManagerLoadsCachedMirrors(t *testing.T) {
	dir := t.TempDir()
	if err := saveMirrors(dir, []mirrorEntry{{Base: "http://203.113.1.7:8080", Location: "Nowhere"}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(20, dir)
	n, at := m.MirrorSummary()
	if n != 1 || at.IsZero() {
		t.Fatalf("没读回缓存的镜像列表: %d %v", n, at)
	}
	// 让缓存过期后列表页又拉不通：还得沿用盘上这份，而不是退回单源
	m.mirrors.portBusy = func(int) bool { return false }
	m.mirrors.fetched = time.Now().Add(-(mirrorListMaxAge + time.Hour))
	t.Setenv("FANOUT_VPNGATE_SITES", statusServer(t, 404).URL)
	if got := m.mirrors.sources(time.Now(), time.Second); len(got) != 2 {
		t.Fatalf("应当沿用缓存的镜像: %v", got)
	}
}

// 启动后的后台聚合、12 小时 ticker、重连退避前的刷新、手动接口可能撞在一起。
// 开 -race 同跑几轮，确认不会互相踩到写出半份列表。
func TestConcurrentRefreshDoesNotTearTheList(t *testing.T) {
	noMirrors(t)
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

// TestLiveRefreshNodes 把生产刷新路径（选源 + 并发拉 + 合并 + 数量检查）跑在真站上。
// 默认跳过；部署前在目标服务器上跑：
//
//	FANOUT_LIVE_TEST=1 FANOUT_VPNGATE_TIMEOUT=60s go test -run TestLiveRefreshNodes -v
//
// 部分镜像不通不算失败（那正是要在目标机器上看的），但合并结果必须过
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
	mirrors, _ := m.MirrorSummary()
	t.Logf("合并后 %d 个节点，镜像列表缓存 %d 个", n, mirrors)
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
