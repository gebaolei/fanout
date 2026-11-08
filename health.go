package main

import (
	"log"
	"net"
	"strings"
	"time"
)

const (
	healthInterval = 10 * time.Second
	healthFailures = 2 // 连续失败几次才判定掉线，避免网络抖动误杀
	healthTimeout  = 6 * time.Second
)

var (
	// 头一次探测失败后，给 openvpn 自愈留出的窗口：每 healStep 复查一次，
	// 最多复查到 healWindow。
	//
	// 必须给：VPN Gate 下发的 ping-restart 很激进，openvpn 会因为几分钟的
	// 拥塞自己重启，实测 3 秒左右恢复。以前不给这段时间，健康检查正好撞上
	// 那几秒 → 判掉线 → 把刚恢复的隧道拆掉 → 又连回同一个节点，
	// 实测每 4 分钟来一轮，每轮十几秒流量从母机泄露。
	//
	// 写成 var（不是 const）只是为了单测能把窗口缩到毫秒级。
	healWindow = 12 * time.Second
	healStep   = 3 * time.Second
)

// WatchHealth 周期检查每条隧道是否还能出网，掉线的自动换节点重连。
// VPN Gate 是志愿者节点，运行中掉线很常见。
func (m *Manager) WatchHealth() {
	fails := map[int]int{}

	for range time.Tick(healthInterval) {
		for _, t := range m.Tunnels() {
			if t.Status != "up" {
				continue
			}
			v := m.checkTunnel(t)
			switch v {
			case healthOK:
				fails[t.Slot] = 0

			case healthLeaked:
				// 出口 IP 变回母机地址：流量正在从母机 NAT 出去。
				// 这种不等自愈，立刻拆 —— 多等一秒就多泄露一秒。
				log.Printf("隧道 %d (%s) 出口变回母机地址，立即换节点重连", t.Slot, t.Node.HostName)
				fails[t.Slot] = 0
				m.autoReconnect(t)

			default:
				fails[t.Slot]++
				// 只在头一次失败时等自愈：真挂掉的节点仍然在两三个探测周期内
				// 被换掉，而不是每次失败都白等 12 秒。
				if fails[t.Slot] == 1 && m.waitForHeal(t) {
					fails[t.Slot] = 0
					continue
				}
				if fails[t.Slot] < healthFailures {
					log.Printf("隧道 %d (%s) 探测失败 %d 次（%s）",
						t.Slot, t.Node.HostName, fails[t.Slot], v)
					continue
				}
				log.Printf("隧道 %d (%s) 已掉线（%s），正在换节点重连",
					t.Slot, t.Node.HostName, v)
				fails[t.Slot] = 0
				m.autoReconnect(t)
			}
		}
	}
}

// autoReconnect 是健康检查触发的重连：先把当前节点记进短期黑名单。
//
// 不加这一步，候选列表永远把原节点排第一位（那是"恢复原样"的语义），
// 于是一条抖动的隧道会一直撞回刚才那个连不上的节点 —— 真机上看到的
// 就是同一个 hostname 每隔几分钟重建一次。
func (m *Manager) autoReconnect(t *Tunnel) {
	t.markAutoFail(t.Node.HostName)
	m.reconnect(t, t.Node.HostName)
}

// waitForHeal 在一次探测失败后再问几次，看隧道是不是自己回来了。
// 回来就返回 true，调用方不该重连。
func (m *Manager) waitForHeal(t *Tunnel) bool {
	deadline := time.Now().Add(healWindow)
	for time.Now().Add(healStep).Before(deadline) {
		time.Sleep(healStep)
		if m.checkTunnel(t) != healthOK {
			continue
		}
		log.Printf("隧道 %d (%s) 探测失败后自行恢复，不重连", t.Slot, t.Node.HostName)
		return true
	}
	return false
}

// healthVerdict 是一次健康检查的结论。四种情况处理方式不同，
// 所以不能只返回 bool —— 全当"掉线"处理会把自愈和泄露都误判。
type healthVerdict int

const (
	healthOK      healthVerdict = iota // 还在原来的出口上
	healthUnknown                      // 查询本身失败或返回空：不知道出口是谁
	healthLeaked                       // 出口变成母机公网 IP：流量正在从母机泄露
	healthChanged                      // 出口变成别的地址（隧道没了，或者节点换了 IP）
)

func (v healthVerdict) String() string {
	switch v {
	case healthOK:
		return "正常"
	case healthLeaked:
		return "出口退回母机"
	case healthChanged:
		return "出口 IP 变了"
	default:
		return "查询失败"
	}
}

// classifyExit 判断"探到的出口 IP"意味着什么。抽成纯函数是为了能单测：
// 真正的探测要 root 和 netns，分类逻辑不需要。
//
//	got     探到的出口 IP（查询失败时为空）
//	want    建立隧道时记下的出口 IP
//	hostIP  母机公网地址，探不到时为空
func classifyExit(got, want, hostIP string) healthVerdict {
	got = strings.TrimSpace(got)
	if got == "" {
		return healthUnknown
	}
	if net.ParseIP(got) == nil {
		// 返回的不是 IP（被门户页、拦截页吃掉），等同于查询没成功
		return healthUnknown
	}
	if got == strings.TrimSpace(want) {
		return healthOK
	}
	// 出口 IP 变了。分不清"退回母机"和"节点自己换了 IP"时都得重连，
	// 但退回母机必须单独认出来：那种情况下流量正在暴露服务器真实地址。
	if hostIP != "" && got == hostIP {
		return healthLeaked
	}
	return healthChanged
}

// reconnect 就地把一条隧道换到别的节点上，保持槽位与端口不变，
// 这样已经分发出去的客户端配置仍然可用。
//
// oldHost 必须是本次重连前那条隧道真正绑着的节点名。调用方若已经
// 改过 t.Node（比如手动换节点），就要把改之前的名字传进来，
// 否则 rebind 找不到旧绑定，入站会掉成孤儿。
func (m *Manager) reconnect(t *Tunnel, oldHost string) {
	t.Status = "starting"
	t.Err = "正在换节点重连"
	t.ExitIP = ""

	// 动手之前先把"原来绑的是谁"落盘。
	//
	// 换节点是两步：连上新节点、再把入站改绑过来。两步之间崩溃或重启的话，
	// 存盘的隧道已经是新节点、入站却还指着旧节点，那个入站就成了孤儿。
	// 这条线索留在盘上，重启恢复时能照着把它接回来。
	t.setPrevHost(oldHost)
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}

	if t.ovpn != nil && t.ovpn.Process != nil {
		_ = t.ovpn.Process.Kill()
		t.ovpn = nil
	}
	t.teardownNetns()

	go func() {
		// 通知延后到 rebind/resync 之后：那两步会把入站改绑到新节点，
		// 提前重建配置会因为入站还指着旧节点名而丢掉路由规则
		m.bringUpPersist(t, false, true)
		if t.Status != "up" {
			return
		}
		// 出站 tag 跟着节点名走，换了节点就要把原来指向它的入站重新绑过去，
		// 否则面板里的路由会指向一个已经不存在的出站。
		if t.Node.HostName != oldHost {
			if err := m.rebind(oldHost, t); err != nil {
				log.Printf("重连后同步 3x-ui 绑定失败: %v", err)
				return
			}
		} else if err := m.resync(t); err != nil {
			// 节点名没变也要重写一次出站：出口 IP 可能变了，
			// 而且上一轮换节点时留下的绑定需要重新指回来。
			log.Printf("重连后重写 3x-ui 出站失败: %v", err)
			return
		}
		// 入站已经跟过来了，标记可以清了
		t.setPrevHost("")
		if err := m.saveState(); err != nil {
			log.Printf("保存状态失败: %v", err)
		}
	}()
}

// healthProbe 是单测留的注入口：非 nil 时取代真实的 netns 查询。
// 真机上始终是 t.queryExitIP；测试里换成普通函数，waitForHeal 才能跑通
// "第一次探失败、紧接着自己好了"这条路径（不必有 root 和 netns）。
var healthProbe func(*Tunnel, time.Duration) (string, error)

// checkTunnel 走一次出口探测并给出结论。
//
// 只看"能不能出网"是不够的：netns 通过 veth 走母机 NAT，openvpn 挂掉后
// 照样能出网，只是出口变回了母机 IP。所以要比对出口 IP 是否还是建隧道时那个。
func (m *Manager) checkTunnel(t *Tunnel) healthVerdict {
	var (
		got string
		err error
	)
	if healthProbe != nil {
		got, err = healthProbe(t, healthTimeout)
	} else {
		got, err = t.queryExitIP(healthTimeout)
	}
	if err != nil {
		return healthUnknown
	}
	// 这里是全程序唯一主动刷新母机公网地址的地方（30 分钟 TTL）。
	// 母机重启换了 IP 而缓存还是旧值的话，泄露会被误判成"出口 IP 变了"，
	// 反而看不出问题。
	return classifyExit(got, t.ExitIP, hostPublicIP())
}
