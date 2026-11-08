package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// withHealWindow 把自愈窗口缩到毫秒级，不然 waitForHeal 的测试要真等 12 秒。
func withHealWindow(t *testing.T, window, step time.Duration) {
	t.Helper()
	pw, ps := healWindow, healStep
	healWindow, healStep = window, step
	t.Cleanup(func() { healWindow, healStep = pw, ps })
}

// withHealthProbe 换掉出口查询的实现，并还原。
func withHealthProbe(t *testing.T, fn func(*Tunnel, time.Duration) (string, error)) {
	t.Helper()
	prev := healthProbe
	healthProbe = fn
	t.Cleanup(func() { healthProbe = prev })
}

// withHostIP 临时指定母机公网地址（健康检查要靠它认泄露）。
func withHostIP(t *testing.T, ip string) {
	t.Helper()
	publicIPMu.Lock()
	prev := publicIPOverride
	publicIPMu.Unlock()
	setPublicIPOverride(ip)
	t.Cleanup(func() { setPublicIPOverride(prev) })
}

func TestClassifyExit(t *testing.T) {
	cases := []struct {
		name            string
		got, want, host string
		expect          healthVerdict
	}{
		{"出口没变", "1.2.3.4", "1.2.3.4", "9.9.9.9", healthOK},
		{"退回母机", "9.9.9.9", "1.2.3.4", "9.9.9.9", healthLeaked},
		{"变成别的地址", "5.5.5.5", "1.2.3.4", "9.9.9.9", healthChanged},
		// 认不出母机地址时不能假装是泄露，只能当"出口变了"重连
		{"不知道母机地址", "5.5.5.5", "1.2.3.4", "", healthChanged},
		{"查询空返回", "", "1.2.3.4", "9.9.9.9", healthUnknown},
		// 被门户页/拦截页吃掉时返回的不是 IP，等同于查询失败
		{"返回的不是 IP", "<html>", "1.2.3.4", "9.9.9.9", healthUnknown},
		{"两边都空", "", "", "9.9.9.9", healthUnknown},
		{"首尾空格", " 1.2.3.4 ", "1.2.3.4", "9.9.9.9", healthOK},
	}
	for _, c := range cases {
		if got := classifyExit(c.got, c.want, c.host); got != c.expect {
			t.Errorf("%s: classifyExit(%q,%q,%q)=%v，想要 %v", c.name, c.got, c.want, c.host, got, c.expect)
		}
	}
}

// 泄露必须能被认出来：这是"隧道还在但流量从母机出去"的唯一信号。
func TestCheckTunnelDetectsLeak(t *testing.T) {
	withHostIP(t, "203.0.113.9")
	withHealthProbe(t, func(*Tunnel, time.Duration) (string, error) { return "203.0.113.9", nil })
	m := NewManager(4, t_tmpdir)
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "vpn1"}, ExitIP: "49.168.191.160", Status: "up"}
	if got := m.checkTunnel(tr); got != healthLeaked {
		t.Fatalf("出口 IP 变成母机地址时应判泄露，实际 %v", got)
	}
}

// 查询失败（超时、curl 报错）不能被判成泄露：那种情况拆隧道没有意义，
// 应该走"给自愈留窗口 + 连败计数"那条路。
func TestCheckTunnelUnknownOnProbeError(t *testing.T) {
	withHostIP(t, "203.0.113.9")
	withHealthProbe(t, func(*Tunnel, time.Duration) (string, error) { return "", errors.New("context deadline exceeded") })
	m := NewManager(4, t_tmpdir)
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "vpn1"}, ExitIP: "49.168.191.160", Status: "up"}
	if got := m.checkTunnel(tr); got != healthUnknown {
		t.Fatalf("查询失败应判 unknown，实际 %v", got)
	}
}

// openvpn 自愈后不该再拆隧道重连：这是这次改动要治的主问题。
func TestWaitForHealAcceptsSelfHeal(t *testing.T) {
	withHealWindow(t, 200*time.Millisecond, 10*time.Millisecond)
	var calls int
	withHealthProbe(t, func(*Tunnel, time.Duration) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("curl: (28) Operation timed out")
		}
		return "49.168.191.160", nil // 几秒后又连上了
	})
	m := NewManager(4, t_tmpdir)
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "vpn1"}, ExitIP: "49.168.191.160", Status: "up"}
	if !m.waitForHeal(tr) {
		t.Fatalf("第二次探测就恢复了，waitForHeal 应返回 true（calls=%d）", calls)
	}
	if calls < 2 {
		t.Fatalf("应该复查过，实际只查了 %d 次", calls)
	}
}

// 一直没恢复时要老老实实返回 false，让调用方计数重连。
func TestWaitForHealGivesUp(t *testing.T) {
	withHealWindow(t, 100*time.Millisecond, 10*time.Millisecond)
	withHealthProbe(t, func(*Tunnel, time.Duration) (string, error) { return "", errors.New("down") })
	m := NewManager(4, t_tmpdir)
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "vpn1"}, ExitIP: "1.2.3.4", Status: "up"}
	if m.waitForHeal(tr) {
		t.Fatal("始终恢复不了时 waitForHeal 应返回 false")
	}
}

func TestAutoFailBlacklistExpires(t *testing.T) {
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "vpn1"}}
	tr.markAutoFail("vpn9")

	now := time.Now()
	if !tr.autoAvoid(now)["vpn9"] {
		t.Fatal("刚标记的节点应在黑名单里")
	}
	if tr.autoAvoid(now.Add(autoFailTTL + time.Minute))["vpn9"] {
		t.Fatal("超过 autoFailTTL 后应自动失效")
	}
	// 空 hostname 不该占位
	tr.markAutoFail("")
	if len(tr.autoAvoid(now)) != 1 {
		t.Fatalf("空 hostname 不该进黑名单，实际 %v", tr.autoAvoid(now))
	}
}

// 标记新节点时要顺手清掉过期的，否则这张表只会一直变长。
func TestAutoFailPrunesStale(t *testing.T) {
	tr := &Tunnel{Slot: 1}
	tr.mu.Lock()
	tr.autoFails = map[string]time.Time{
		"old": time.Now().Add(-2 * autoFailTTL),
	}
	tr.mu.Unlock()
	tr.markAutoFail("new")
	if len(tr.autoAvoid(time.Now())) != 1 {
		t.Fatalf("过期条目应被清掉，实际 %v", tr.autoFails)
	}
}

// 刚连不上的原节点不能再排第一，否则就是"拆掉→又连回同一个→再拆"。
func TestCandidatesForSkipsRecentlyFailedNode(t *testing.T) {
	m := mgrWith(sample)
	tr := &Tunnel{Slot: 9, Node: sample[0]} // jp1
	tr.markAutoFail("jp1")

	got := m.candidatesFor(tr)
	if len(got) == 0 {
		t.Fatal("应当还有候选")
	}
	for _, n := range got {
		if n.HostName == "jp1" {
			t.Fatalf("刚失败过的 jp1 不该出现在候选里，实际 %+v", hostNames(got))
		}
	}
	if got[0].HostName != "jp2" {
		t.Fatalf("应改为先试同地区的 jp2，实际 %s", got[0].HostName)
	}
}

// 没标记过时保持原语义：先试原节点，目标是把这条出口恢复原样。
func TestCandidatesForPrefersOriginalNode(t *testing.T) {
	m := mgrWith(sample)
	tr := &Tunnel{Slot: 9, Node: sample[0]}
	if got := m.candidatesFor(tr); got[0].HostName != "jp1" {
		t.Fatalf("默认应先试原节点，实际 %s", hostNames(got))
	}
}

// 同地区只剩它自己时，黑名单不能把隧道饿死。
func TestCandidatesForFallsBackToOnlyCandidate(t *testing.T) {
	only := []Node{{HostName: "jp1", CountryCode: "JP", Country: "Japan", Residential: true}}
	m := mgrWith(only)
	tr := &Tunnel{Slot: 9, Node: only[0]}
	tr.markAutoFail("jp1")

	got := m.candidatesFor(tr)
	if len(got) != 1 || got[0].HostName != "jp1" {
		t.Fatalf("只剩一个候选时应回到它，实际 %+v", hostNames(got))
	}
}

// 手动换节点的历史（swapped）与自动黑名单互不污染：
// 用户明确不要的 IP 一直避开，自动失败的只是短期不优先。
func TestAutoFailDoesNotTouchSwapHistory(t *testing.T) {
	tr := &Tunnel{Slot: 1, Node: Node{HostName: "a"}, swapped: []string{"b"}}
	tr.markAutoFail("c")
	if !tr.swapAvoid()["b"] || !tr.swapAvoid()["a"] {
		t.Fatalf("手动换节点历史不该被影响，实际 %v", tr.swapAvoid())
	}
	if tr.swapAvoid()["c"] {
		t.Fatal("自动失败的节点不该进手动换节点历史")
	}
	if !tr.autoAvoid(time.Now())["c"] {
		t.Fatal("自动失败应进短期黑名单")
	}
}

func TestOvpnKeepalive(t *testing.T) {
	cases := []struct {
		env           string
		ping, restart int
		ok            bool
	}{
		{"", 0, 0, false},           // 显式设空＝完全沿用上游推的值
		{"off", 0, 0, false},        //
		{"none", 0, 0, false},       //
		{"10/60", 10, 60, true},     // 自定义
		{"20 / 300", 20, 300, true}, // 带空格也认
		{"abc", 15, 120, true},      // 看不懂就用默认
		{"60/15", 15, 120, true},    // restart 不比 ping 大，数值不合理，退回默认
		{"15", 15, 120, true},       // 缺斜杠也退回默认
	}
	for _, c := range cases {
		t.Setenv("FANOUT_OVPN_KEEPALIVE", c.env)
		p, r, ok := ovpnKeepalive()
		if p != c.ping || r != c.restart || ok != c.ok {
			t.Errorf("FANOUT_OVPN_KEEPALIVE=%q 得到 %d/%d ok=%v，想要 %d/%d ok=%v",
				c.env, p, r, ok, c.ping, c.restart, c.ok)
		}
	}
}

// 没设这个环境变量时也必须给出默认值（线上就是靠这条生效）。
func TestOvpnKeepaliveDefaultWithoutEnv(t *testing.T) {
	prev, had := os.LookupEnv("FANOUT_OVPN_KEEPALIVE")
	if err := os.Unsetenv("FANOUT_OVPN_KEEPALIVE"); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("FANOUT_OVPN_KEEPALIVE", prev)
		}
	})
	p, r, ok := ovpnKeepalive()
	if !ok || p != 15 || r != 120 {
		t.Fatalf("默认应为 15/120 且启用，实际 %d/%d ok=%v", p, r, ok)
	}
}

// 第三方池的节点（.ovpn 要去 CDN 现取）不能抢在源站实时节点前面。
//
// 节点列表是按速度降序合并的，池子那些“7 天快照里分数很高”的节点会插到
// 实时节点前面；而它们只保证“六小时前 Cloudflare 边缘 TCP 探得过”。
// 自动重连要的是能连回来，不是分数高。
func TestCandidatesForPrefersLiveNodes(t *testing.T) {
	ref := func(p string) *configRef {
		return &configRef{Commit: strings.Repeat("a", 40), Path: p, SHA256: strings.Repeat("b", 64)}
	}
	nodes := []Node{
		{HostName: "vpn900001", CountryCode: "JP", SpeedMbps: 900, Residential: true, RemoteConfig: ref("pool/configs/1.json")},
		{HostName: "vpn900002", CountryCode: "JP", SpeedMbps: 800, Residential: true, RemoteConfig: ref("pool/configs/2.json")},
		{HostName: "jp1", CountryCode: "JP", SpeedMbps: 300, Residential: true, Config: "client\nremote 1.1.1.1 443\n"},
		{HostName: "jp2", CountryCode: "JP", SpeedMbps: 200, Residential: true, Config: "client\nremote 1.1.1.2 443\n"},
	}
	m := mgrWith(nodes)
	// 原节点不在新列表里（重建后下线的常见情形），所以候选从列表开头挑。
	tr := &Tunnel{Slot: 9, Node: Node{HostName: "jp0", CountryCode: "JP"}}

	got := hostNames(m.candidatesFor(tr))
	want := []string{"jp0", "jp1", "jp2", "vpn900001", "vpn900002"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("实时节点该排在池子节点前面，实际 %v", got)
	}
}

// 实时节点不够时，池子节点要能补上来（不然接入这个源就没意义了）。
func TestCandidatesForFallsBackToPoolNodes(t *testing.T) {
	nodes := []Node{
		{HostName: "jp1", CountryCode: "JP", SpeedMbps: 300, Residential: true, Config: "client\n"},
		{HostName: "vpn900001", CountryCode: "JP", SpeedMbps: 900, Residential: true,
			RemoteConfig: &configRef{Commit: strings.Repeat("a", 40), Path: "pool/configs/1.json", SHA256: strings.Repeat("b", 64)}},
		{HostName: "vpn900002", CountryCode: "JP", SpeedMbps: 800, Residential: true,
			RemoteConfig: &configRef{Commit: strings.Repeat("a", 40), Path: "pool/configs/2.json", SHA256: strings.Repeat("b", 64)}},
	}
	m := mgrWith(nodes)
	tr := &Tunnel{Slot: 9, Node: Node{HostName: "jp0", CountryCode: "JP"}}
	tr.markAutoFail("jp1") // 唯一的实时节点刚连不上

	got := hostNames(m.candidatesFor(tr))
	want := []string{"jp0", "vpn900001", "vpn900002"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("实时节点被挡掉时应该用池子节点，实际 %v", got)
	}
}

func hostNames(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.HostName)
	}
	return out
}
