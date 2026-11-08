package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 换节点未收尾的标记要跟着状态一起落盘。
//
// 不存的话，重启后隧道是新节点、入站还绑着旧节点，两边对不上，
// 那个入站就掉成没人认领的孤儿，流量悄悄走直连。
func TestSaveStateKeepsPrevHost(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(20, dir)
	tn := &Tunnel{
		Slot: 1, Port: 12345,
		Node:   Node{HostName: "vpn-new", CountryCode: "JP"},
		Status: "starting",
		Cred:   SocksCred{User: "u", Pass: "p"},
	}
	tn.setPrevHost("vpn-old")
	m.tunnels[1] = tn

	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st persistedState
	if err := json.Unmarshal(blob, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Tunnels) != 1 {
		t.Fatalf("应当存下 1 条，实际 %d", len(st.Tunnels))
	}
	if st.Tunnels[0].PrevHost != "vpn-old" {
		t.Fatalf("没存下换节点前绑的节点，实际 %q", st.Tunnels[0].PrevHost)
	}
	if st.Tunnels[0].HostName != "vpn-new" {
		t.Fatalf("当前节点应当是新的，实际 %q", st.Tunnels[0].HostName)
	}
}

// 收尾之后标记要清掉，否则每次重启都会白做一次改绑。
func TestPrevHostClearedAfterSettle(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(20, dir)
	tn := &Tunnel{Slot: 1, Node: Node{HostName: "vpn-new"}, Status: "up"}
	tn.setPrevHost("vpn-old")
	m.tunnels[1] = tn
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}

	tn.setPrevHost("")
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var st persistedState
	if err := json.Unmarshal(blob, &st); err != nil {
		t.Fatal(err)
	}
	if st.Tunnels[0].PrevHost != "" {
		t.Fatalf("收尾后标记该清掉，实际 %q", st.Tunnels[0].PrevHost)
	}
	// omitempty：清掉之后不该再出现在文件里
	if got := string(blob); containsStr(got, "prev_host") {
		t.Fatalf("清掉之后字段不该还在文件里: %s", got)
	}
}

// 恢复时要把标记读回来，否则自愈逻辑根本不会触发。
func TestRestoreStateReadsPrevHost(t *testing.T) {
	dir := t.TempDir()
	blob := `{"tunnels":[{"slot":1,"port":12345,"hostname":"vpn-new","country_code":"JP","config":"x","socks_user":"u","socks_pass":"p","prev_host":"vpn-old"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(20, dir)
	// 不让它真去拉隧道：restoreTunnel 会调 bringUpPersist，
	// 这里只关心状态读没读对，所以读完立刻看字段。
	n, err := m.restoreState()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应当恢复 1 条，实际 %d", n)
	}
	m.mu.RLock()
	tn := m.tunnels[1]
	m.mu.RUnlock()
	if tn == nil {
		t.Fatal("隧道没恢复出来")
	}
	if got := tn.prevHostOf(); got != "vpn-old" {
		t.Fatalf("没读回换节点标记，实际 %q", got)
	}
	// 收尾：别让后台的 bringUpPersist 继续折腾
	tn.Status = "stopped"
}

// 老版本的状态文件里没有这个字段，读出来该是空的，不能误触发改绑。
func TestRestoreStateOldFormatHasNoPrevHost(t *testing.T) {
	dir := t.TempDir()
	blob := `{"tunnels":[{"slot":1,"port":12345,"hostname":"vpn-a","country_code":"JP","config":"x","socks_user":"u","socks_pass":"p"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(20, dir)
	if _, err := m.restoreState(); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	tn := m.tunnels[1]
	m.mu.RUnlock()
	if got := tn.prevHostOf(); got != "" {
		t.Fatalf("老格式不该带标记，实际 %q", got)
	}
	tn.Status = "stopped"
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 跑在第三方池节点上的隧道，重启后还要能取到 .ovpn。
//
// 池子列表只是一个 commit 的快照，重启后这个节点可能已经不在列表里；而它的
// .ovpn 不像源站节点那样内联在列表里。不存引用的话，恢复出来就是一个空
// Config，永远连不上 —— 真机上那个"候选 vpn200460917 连不上：没有 .ovpn
// 配置"的死候选就是这么来的。
func TestSaveStateKeepsPoolConfigRef(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(20, dir)
	tn := &Tunnel{
		Slot: 4, Port: 22059, Status: "up",
		Cred: SocksCred{User: "u", Pass: "p"},
		Node: Node{HostName: "vpn900001", CountryCode: "US",
			RemoteConfig: &configRef{Commit: strings.Repeat("a", 40), Path: "pool/configs/x.json", SHA256: strings.Repeat("b", 64)}},
	}
	m.tunnels[4] = tn
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}

	// 换个 manager 从盘上读，并且当前列表里没有这个节点（模拟它已从池子里过期）
	m2 := NewManager(20, dir)
	if _, err := m2.restoreState(); err != nil {
		t.Fatal(err)
	}
	m2.mu.RLock()
	got := m2.tunnels[4]
	m2.mu.RUnlock()
	if got == nil {
		t.Fatal("隧道没恢复出来")
	}
	got.Status = "stopped"
	if got.Node.RemoteConfig == nil {
		t.Fatal("池子节点的 .ovpn 引用没跟着状态落盘")
	}
	ref := got.Node.RemoteConfig
	if ref.Commit != strings.Repeat("a", 40) || ref.Path != "pool/configs/x.json" || ref.SHA256 != strings.Repeat("b", 64) {
		t.Errorf("落盘的引用不完整: %+v", ref)
	}
}

// 存盘那一刻 .ovpn 还没取到（上一轮重连刚把候选换成池子节点就被重启），
// 恢复后要能照存盘的引用把配置取回来，而不是变成一个连不上的死候选。
func TestRestoreStateRebuildsMissingConfigFromRef(t *testing.T) {
	dir := t.TempDir()
	blob := `{"tunnels":[{"slot":4,"port":22059,"hostname":"vpn900001","country_code":"US","config":"",` +
		`"config_commit":"` + strings.Repeat("a", 40) + `","config_path":"pool/configs/x.json",` +
		`"config_sha256":"` + strings.Repeat("b", 64) + `","socks_user":"u","socks_pass":"p"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(20, dir)
	if _, err := m.restoreState(); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	tn := m.tunnels[4]
	m.mu.RUnlock()
	tn.Status = "stopped"
	if tn.Node.RemoteConfig == nil {
		t.Fatal("空 Config + 存盘引用应当能恢复出取数入口")
	}
	if tn.autoAvoid(time.Now())[tn.Node.HostName] {
		t.Error("能靠引用取到配置的节点不该被当成死候选")
	}
}

// 既没有 .ovpn 又没有引用（老状态文件，或者那个池子节点已经过期）：
// 这种节点永远连不上，却会占掉一个候选名额（一次 40 多秒）和 6 分钟黑名单。
// 直接当"刚连不上"处理，让同地区其他节点顶上来。
func TestRestoreStateWithoutAnyConfigSkipsCandidate(t *testing.T) {
	dir := t.TempDir()
	blob := `{"tunnels":[{"slot":4,"port":22059,"hostname":"vpn-gone","country_code":"US","config":"",` +
		`"socks_user":"u","socks_pass":"p"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(20, dir)
	m.nodes = []Node{{HostName: "vpn-us", CountryCode: "US", SpeedMbps: 100, Residential: true, Config: "client\n"}}
	if _, err := m.restoreState(); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	tn := m.tunnels[4]
	m.mu.RUnlock()
	tn.Status = "stopped"
	if !tn.autoAvoid(time.Now())["vpn-gone"] {
		t.Fatal("没有配置来源的原节点该进短期黑名单")
	}
	if got := hostNames(m.candidatesFor(tn)); len(got) != 1 || got[0] != "vpn-us" {
		t.Errorf("候选里只剩那个能连的节点才对，实际 %v", got)
	}
}
