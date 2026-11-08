package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// persistedTunnel 是隧道在磁盘上的形态。
// 只存重建所需的信息，运行态（netns、进程、监听）重启后重新建立。
type persistedTunnel struct {
	Slot        int    `json:"slot"`
	Port        int    `json:"port"`
	HostName    string `json:"hostname"`
	CountryCode string `json:"country_code"`
	Country     string `json:"country"`
	Config      string `json:"config"`
	// 第三方节点池的 .ovpn 引用。跑在池子节点上的隧道必须把它存下来：
	// 列表只是一个 commit 的快照，重启后这个节点可能已经不在当前列表里，
	// 而它的 .ovpn 又不像源站节点那样内联在列表里。不存的话恢复出来就是个
	// 空 Config，谁也连不上（真机上撞到过：候选 xxx 连不上：没有 .ovpn 配置）。
	ConfigCommit string `json:"config_commit,omitempty"`
	ConfigPath   string `json:"config_path,omitempty"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	// SOCKS5 凭据要存盘：用户已经把它分发给客户端了，重启后变掉等于全断
	SocksUser string `json:"socks_user,omitempty"`
	SocksPass string `json:"socks_pass,omitempty"`
	// PrevHost 非空表示上次换节点只做了一半：隧道已经指向新节点，
	// 但入站还绑在这个旧节点上。恢复时照着它把入站接回来。
	PrevHost string `json:"prev_host,omitempty"`
}

type persistedState struct {
	Tunnels []persistedTunnel `json:"tunnels"`
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// saveState 把当前隧道写入磁盘，供重启后恢复。
func (m *Manager) saveState() error {
	var st persistedState
	for _, t := range m.Tunnels() {
		// 只跳过用户主动停掉的。starting/failed 的隧道也要存：
		// 它们正在重连或等着重试，漏存会让重启后凭空少几个出口。
		if t.Status == "stopped" {
			continue
		}
		st.Tunnels = append(st.Tunnels, persistedTunnel{
			Slot:        t.Slot,
			Port:        t.Port,
			HostName:    t.Node.HostName,
			CountryCode: t.Node.CountryCode,
			Country:     t.Node.Country,
			Config:      t.Node.Config,
			SocksUser:   t.Cred.User,
			SocksPass:   t.Cred.Pass,
			PrevHost:    t.prevHostOf(),
		})
		if ref := t.Node.RemoteConfig; ref != nil {
			n := len(st.Tunnels) - 1
			st.Tunnels[n].ConfigCommit = ref.Commit
			st.Tunnels[n].ConfigPath = ref.Path
			st.Tunnels[n].ConfigSHA256 = ref.SHA256
		}
	}

	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(m.workDir) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(m.workDir))
}

// restoreState 读回上次的隧道并逐条拉起。
// 节点配置一并存了盘，所以即使 VPN Gate 列表里该节点已消失也能重建。
func (m *Manager) restoreState() (int, error) {
	blob, err := os.ReadFile(statePath(m.workDir))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var st persistedState
	if err := json.Unmarshal(blob, &st); err != nil {
		return 0, fmt.Errorf("解析状态文件失败: %w", err)
	}

	// 从当前节点列表补回地区、延迟等元数据；节点已下线时退回存盘的最小信息
	known := map[string]Node{}
	for _, n := range m.nodes {
		known[n.HostName] = n
	}

	for _, p := range st.Tunnels {
		node, ok := known[p.HostName]
		if !ok {
			// 节点已从 VPN Gate 列表消失，用存盘的信息重建
			node = Node{
				HostName:    p.HostName,
				CountryCode: p.CountryCode,
				Country:     p.Country,
			}
		}
		node.Config = p.Config
		// 存盘时还没取到 .ovpn 的池子节点（重连只做了一半就被重启），照着存盘
		// 的引用把取数入口接回来。不这么做的话这条隧道恢复出来就是个死候选。
		if node.Config == "" && node.RemoteConfig == nil && p.ConfigPath != "" {
			node.RemoteConfig = &configRef{Commit: p.ConfigCommit, Path: p.ConfigPath, SHA256: p.ConfigSHA256}
		}
		// 从旧版本升上来的状态文件没有凭据字段，补一套新的
		cred := SocksCred{User: p.SocksUser, Pass: p.SocksPass}
		if cred.User == "" || cred.Pass == "" {
			gen, err := newSocksCred()
			if err != nil {
				return 0, fmt.Errorf("生成 SOCKS5 凭据失败: %w", err)
			}
			cred = gen
		}
		t := &Tunnel{
			Slot:   p.Slot,
			Port:   p.Port,
			Node:   node,
			Status: "starting",
			Cred:   cred,
		}
		t.setPrevHost(p.PrevHost)
		if t.Node.Config == "" && t.Node.RemoteConfig == nil {
			// 存盘记录里既没有 .ovpn，也没有可取的引用：上一次重连只做到一半就
			// 重启了，或者那个池子节点已经过期。这种节点永远连不上，却会占掉一个
			// 候选名额（一次 40 多秒）外加 6 分钟黑名单，所以直接当“刚连不上”处理，
			// 让同地区其他节点顶上来。
			log.Printf("槽 %d 的落盘记录里没有 %s 的 .ovpn，先试同地区其他节点",
				p.Slot, p.HostName)
			t.markAutoFail(p.HostName)
		}
		m.mu.Lock()
		m.tunnels[p.Slot] = t
		m.mu.Unlock()
		go m.restoreTunnel(t)
	}
	return len(st.Tunnels), nil
}

// restoreTunnel 拉起一条恢复出来的隧道，顺手把上次没做完的换节点收尾。
func (m *Manager) restoreTunnel(t *Tunnel) {
	prev := t.prevHostOf()
	m.bringUpPersist(t, true, true)
	if prev == "" || t.Status != "up" {
		return
	}
	// 上次换节点改完隧道就中断了，入站还指着旧节点。不接回来的话它会一直
	// 显示成"未绑定出口"，流量悄悄走直连——用户看不出哪里不对。
	if err := m.rebind(prev, t); err != nil {
		log.Printf("恢复时把入站接回出口 %d 失败: %v", t.Slot, err)
		return
	}
	log.Printf("出口 %d 上次换节点没收尾，已把原来绑着 %s 的入站接到 %s",
		t.Slot, prev, t.Node.HostName)
	t.setPrevHost("")
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
}
