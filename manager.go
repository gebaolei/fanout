package main

import (
	"fmt"
	"log"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Manager 维护所有隧道，负责分配槽位与端口。
type Manager struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex // 串行化节点列表刷新（见 RefreshNodes）
	tunnels   map[int]*Tunnel
	nodes     []Node
	fetched   time.Time
	// lastTry 是最近一次发起拉取的时间（不论成败），给重连循环里的冷却用。
	// 不能用 fetched：全部源失败时 fetched 不动，冷却就失效了。
	lastTry time.Time
	// stats 是最近一轮各源的拉取结果，只用于日志与界面诊断
	stats []sourceStat
	// sourceCounts 记住每个源上一轮各自返了多少节点，用作下一轮“骤减”
	// 判断的基准（按 URL 各自比；合并后的总数比单源大得多，不能混用）。
	sourceCounts map[string]int
	mirrors      *mirrorStore
	// pool 是第三方滚动节点池（见 poolsource.go）。与 mirrors 一样是可选源：
	// 它挂了不影响这一轮，主站被限流时它能顶上。
	pool     *poolStore
	workDir  string
	maxSlots int
	jobs     JobStore
}

func NewManager(maxSlots int, workDir string) *Manager {
	return &Manager{
		tunnels:      map[int]*Tunnel{},
		workDir:      workDir,
		maxSlots:     maxSlots,
		mirrors:      newMirrorStore(workDir),
		pool:         newPoolStore(workDir),
		sourceCounts: map[string]int{},
	}
}

// RefreshNodes 多源拉取并合并节点列表。
//
// 任一源成功即算成功；全部失败时返回错误并且不动 m.nodes，
// 保留上一轮的列表（这个语义改动前就有，多源后必须保持）。
//
// refreshMu 串行化整轮：启动后的后台聚合、12 小时 ticker、重连退避前的
// 刷新、手动接口可能撞在一起。并发跑两轮不会错，但会白白多打一轮带宽，
// 还会把轮转偏移一次跳两格。
func (m *Manager) RefreshNodes() (int, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	timeout := nodeFetchTimeout()
	m.mu.Lock()
	m.lastTry = time.Now()
	m.mu.Unlock()

	m.mu.RLock()
	prev := len(m.nodes)
	prevByURL := make(map[string]int, len(m.sourceCounts))
	for u, c := range m.sourceCounts {
		prevByURL[u] = c
	}
	m.mu.RUnlock()

	// 经 NewManager 构造的都有 mirrorStore；手构的 Manager（测试里）退回单源。
	sources := []string{vpngateAPI}
	if m.mirrors != nil {
		sources = m.mirrors.sources(time.Now(), timeout)
	}
	// 第三方节点池与 CSV 源并发拉：它最坏要 60 秒（7.1MB），串在前面会把
	// 整轮刷新拖慢一倍。两边失败互不影响。
	type poolResult struct {
		nodes []Node
		stat  sourceStat
		err   error
	}
	poolDone := make(chan poolResult, 1)
	go func() {
		if m.pool == nil {
			poolDone <- poolResult{}
			return
		}
		nodes, stat, err := m.pool.get(time.Now(), poolFetchTimeout())
		poolDone <- poolResult{nodes: nodes, stat: stat, err: err}
	}()

	nodes, stats, err := fetchAggregated(sources, timeout, prevByURL)
	pr := <-poolDone
	if pr.stat.URL != "" {
		// 池源也进同一份统计：界面与日志不用为它特判一个字段
		stats = append(stats, pr.stat)
	}
	if err != nil {
		// 失败也要把各源结果存下来：界面要能看出是“三个源都拉不动”还是“只有主站挂了”
		m.mu.Lock()
		m.stats = stats
		m.mu.Unlock()
		if m.mirrors != nil {
			m.mirrors.noteFailure(time.Now())
		}
		log.Printf("节点列表刷新失败（%d 个源全部不可用）: %v", len(stats), err)
		for _, s := range stats {
			log.Printf("  源 %s: %s", s.URL, s.Err)
		}
		if len(pr.nodes) == 0 {
			return 0, err
		}
		// CSV 源全部失败（主站限流时真会这样），但第三方池里有货：
		// 用它顶上。节点会旧半小时以上，总比告诉用户“没节点”强。
		log.Printf("CSV 节点源全部失败，本轮改用第三方节点池的 %d 个节点", len(pr.nodes))
	}
	if len(pr.nodes) > 0 {
		// CSV 排在前面：它是实时的，池子是缓存快照。mergeNodeSources 按
		// hostname 去重时保留先出现的那条，所以同名节点沿用 CSV 的 Ping/Speed。
		nodes = mergeNodeSources([][]Node{nodes, pr.nodes})
	}
	if len(nodes) == 0 {
		return 0, fmt.Errorf("所有节点源都失败")
	}
	ok := 0
	counts := make(map[string]int, len(stats))
	for _, s := range stats {
		if s.Err == "" {
			ok++
			counts[s.URL] = s.Nodes
		}
	}
	m.mu.Lock()
	m.nodes = nodes
	m.fetched = time.Now()
	m.stats = stats
	// 只记成功源的数：失败源没有可信基准，拿它比会把正常波动当成异常
	m.sourceCounts = counts
	m.mu.Unlock()
	mirrors, _ := m.MirrorSummary()
	poolN, _ := m.PoolSummary()
	log.Printf("节点列表刷新完成：源 %d/%d 成功，合并后 %d 个节点（上一轮 %d），镜像列表缓存 %d 个，第三方池 %d 个",
		ok, len(stats), len(nodes), prev, mirrors, poolN)
	// 部分成功时也要把挂掉的源和原因打出来。“源 1/3”不告诉你剩下两个是
	// “镜像站本身挂了”还是“这台机器到不了非 80/443 的明文端口”，
	// 而这两种情况的处理办法完全不同。
	for _, s := range stats {
		if s.Err != "" {
			log.Printf("  源 %s 未取到（%dms）: %s", s.URL, s.MS, s.Err)
		}
	}
	return len(nodes), nil
}

// retryRefresh 是重连循环里的节点列表刷新：maxAge 内已经拉过就直接跳过。
//
// 不加这个冷却的话，一条连不上的隧道每轮重试都要打一轮三个源：实测一小时
// 上百次，把主站和自建反代都打进限流（主站改回 HTML、反代直回 429），
// 等到真需要刷新列表时反而刷不动。而候选节点在这一分钟内根本不会变。
func (m *Manager) retryRefresh(maxAge time.Duration) {
	m.mu.RLock()
	last := m.lastTry
	m.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < maxAge {
		return
	}
	if _, err := m.RefreshNodes(); err != nil {
		log.Printf("重试前刷新节点列表失败: %v", err)
	}
}

// RefreshMainNodes 只拉主站（失败退到自建反代），即多源之前的单源行为。
//
// 启动时用：节点池能在一个单源超时内就位，而 restoreState 不必排在
// 多源拉取后面 —— 后者最坏能把所有出口隧道的恢复推迟近一分钟。
func (m *Manager) RefreshMainNodes() (int, error) {
	nodes, err := fetchNodes(nodeFetchTimeout())
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.nodes = nodes
	m.fetched = time.Now()
	m.mu.Unlock()
	return len(nodes), nil
}

// WarmNodePool 在后台补一轮多源聚合，把池子从“只有主站那一百来个”扩开。
// 启动路径调它，所以出口恢复不等镜像。
func (m *Manager) WarmNodePool() {
	if n, err := m.RefreshNodes(); err != nil {
		log.Printf("后台聚合节点列表失败: %v", err)
	} else {
		log.Printf("后台聚合完成：%d 个节点", n)
	}
}

// nodeRefreshInterval 是定时刷新的间隔。取 12 小时而不是十几分钟：
// 镜像本身有缓存，连续拉同一个返回逐字节相同的响应。
const nodeRefreshInterval = 12 * time.Hour

// WatchNodes 每 12 小时刷新一轮节点列表。
//
// 没有这个定时器时，刷新只发生在启动、“自动重连且一整轮候选全挂”、
// 以及手动接口三处。出口都正常的机器永远不会触发前者，于是池子会
// 随时间单调变旧（聚合又会带入更多缓存快照里的节点）。
//
// 定时刷新不碰 notifyPanel：节点列表变化不影响 Xray 配置，
// 只有隧道集合变化才影响。
func (m *Manager) WatchNodes() {
	t := time.NewTicker(nodeRefreshInterval)
	defer t.Stop()
	for range t.C {
		n, err := m.RefreshNodes()
		if err != nil {
			log.Printf("定时刷新节点列表失败: %v", err)
			continue
		}
		log.Printf("定时刷新节点列表：当前 %d 个", n)
	}
}

// NodeSources 返回最近一轮各源的拉取结果。
func (m *Manager) NodeSources() []sourceStat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]sourceStat, len(m.stats))
	copy(out, m.stats)
	return out
}

// MirrorSummary 返回镜像列表缓存数与列表页上次更新时间。
func (m *Manager) MirrorSummary() (int, time.Time) {
	if m.mirrors == nil {
		return 0, time.Time{}
	}
	return m.mirrors.summary()
}

// PoolSummary 返回第三方节点池里可用节点数，以及上游最后一次抓源站的时间。
func (m *Manager) PoolSummary() (int, time.Time) {
	if m.pool == nil {
		return 0, time.Time{}
	}
	return m.pool.summary()
}

func (m *Manager) Nodes() ([]Node, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Node, len(m.nodes))
	copy(out, m.nodes)
	return out, m.fetched
}

func (m *Manager) Tunnels() []*Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// freeSlot 找一个未占用的槽位。槽位同时决定端口与网段。
func (m *Manager) freeSlot() (int, error) {
	for i := 1; i <= m.maxSlots; i++ {
		if _, used := m.tunnels[i]; !used {
			return i, nil
		}
	}
	return 0, fmt.Errorf("槽位已满（上限 %d）", m.maxSlots)
}

// Start 为指定节点开一条隧道，返回分配到的本地端口。
func (m *Manager) Start(node Node) (*Tunnel, error) {
	m.mu.Lock()
	slot, err := m.freeSlot()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// 端口随机取，避免固定规律撞上机器上的其他服务
	taken := map[int]bool{}
	for _, other := range m.tunnels {
		taken[other.Port] = true
	}
	port, err := freeRandomPort(taken)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	cred, err := newSocksCred()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	t := &Tunnel{
		Slot:   slot,
		Port:   port,
		Node:   node,
		Status: "starting",
		Since:  time.Now(),
		Cred:   cred,
	}
	m.tunnels[slot] = t
	m.mu.Unlock()

	go m.bringUp(t, true)
	return t, nil
}

// bringUp 把一条隧道拉起来。
//
// notify 决定成功后是否立刻重建后端配置。换节点重连时要传 false：
// 那条路径随后会调 rebind/resync 把入站改绑到新节点，在那之前重建配置
// 会因为入站还指着旧节点名而把路由规则丢掉。
func (m *Manager) bringUp(t *Tunnel, notify bool) {
	m.bringUpPersist(t, notify, false)
}

// 自动重连的退避区间：一轮候选全挂后等一会儿再重试。
//
// 上限取 5 分钟而不是 60 秒：一条连不上的隧道会一直循环，60 秒封顶时
// 每小时要跑三十多轮，每轮还要打一轮节点列表（实测主站和自建反代
// 都被这样打进限流：主站回 HTML、反代回 429），而候选列表在 1 分钟内
// 不会变。头几轮仍然是 5/10/20/40/80/160 秒，真故障仍能快速重试。
const (
	reconnectBackoffMin = 5 * time.Second
	reconnectBackoffMax = 5 * time.Minute
)

// refreshRetryCooldown 是重连循环里两次拉取节点列表的最小间隔。
// 重连靠的是换候选节点，不是换列表；列表本身 12 小时才需要刷一轮。
const refreshRetryCooldown = 10 * time.Minute

// bringUpPersist 把一条隧道拉起来。
//
// persist=false（手动新建）：走一轮候选，全失败就标 failed，让用户能立刻看到并重试。
// persist=true（自动重连 / 重启恢复）：一轮全失败不放弃，退避后刷新节点列表再来一轮，
// 一直循环到连上或这条隧道被用户停掉。VPN Gate 死节点多，"当前都不可用"往往只是
// 这一批候选恰好都挂了，过一会儿就有新节点，不该让出口永久躺死。
func (m *Manager) bringUpPersist(t *Tunnel, notify bool, persist bool) {
	backoff := reconnectBackoffMin
	for {
		if m.tryCandidates(t, notify) {
			return
		}
		// 隧道已被用户停掉或从管理器移除，别再重试
		if !persist || !m.tunnelActive(t) {
			if persist {
				return
			}
			t.Status = "failed"
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			return
		}

		t.Status = "starting"
		t.Err = fmt.Sprintf("暂无可用节点，%.0f 秒后重试", backoff.Seconds())
		log.Printf("隧道 %d 一轮候选均失败，%.0f 秒后重试", t.Slot, backoff.Seconds())
		time.Sleep(backoff)
		if !m.tunnelActive(t) {
			return
		}
		m.retryRefresh(refreshRetryCooldown)
		if backoff < reconnectBackoffMax {
			backoff *= 2
			if backoff > reconnectBackoffMax {
				backoff = reconnectBackoffMax
			}
		}
	}
}

// tryCandidates 走一轮候选节点，成功返回 true。失败不改 Status（留给调用方决定）。
func (m *Manager) tryCandidates(t *Tunnel, notify bool) bool {
	// VPN Gate 是志愿者节点，列表里有相当比例已下线或满员（AUTH_FAILED），
	// 连不上就顺着候选列表换下一个，不必让用户手动试。
	candidates := m.candidatesFor(t)
	for i, node := range candidates {
		if !m.tunnelActive(t) {
			return false
		}
		// 其他隧道可能在重试期间占用了这个节点，跳过以免多个端口撞同一出口 IP
		if i > 0 && m.nodeInUse(node.HostName, t.Slot) {
			continue
		}
		t.Node = node
		t.Status = "starting"
		if i > 0 {
			t.Err = fmt.Sprintf("已换到第 %d 个候选节点", i+1)
		}

		err := m.tryNode(t)
		if err == nil {
			t.Status = "up"
			t.Err = ""
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			if notify {
				m.notifyPanel()
			}
			return true
		}
		// 短期内别再把它当首选：候选轮完一圈又撞回同一个连不上的节点，
		// 就变成真机上那种“同一个 hostname 每几分钟重建一次”。
		t.markAutoFail(node.HostName)
		t.teardownNetns()
	}
	return false
}

// tunnelActive 判断这条隧道是否还归管理器所有且未被用户停掉。
// 用指针比对：Stop 会从 map 里删除并把 Status 置 stopped，
// 重连循环据此退出，避免对着一条已经不存在的隧道空转。
func (m *Manager) tunnelActive(t *Tunnel) bool {
	if t.Status == "stopped" {
		return false
	}
	m.mu.RLock()
	cur, ok := m.tunnels[t.Slot]
	m.mu.RUnlock()
	return ok && cur == t
}

// tryNode 尝试用当前节点把隧道拉起来。
func (m *Manager) tryNode(t *Tunnel) error {
	// 第三方池的节点不在列表里内联 .ovpn，到这一步才去取（13KB）。
	// 放在这里而不是拉列表时取：一轮拉 600 多个节点就是 8MB，
	// 而实际只会连上其中一两个。
	if t.Node.Config == "" && t.Node.RemoteConfig != nil {
		if m.pool == nil {
			return fmt.Errorf("节点 %s 的配置要去第三方节点池取，但这个源已被关闭", t.Node.HostName)
		}
		cfg, err := m.pool.config(t.Node, poolFetchTimeout())
		if err != nil {
			return err
		}
		t.Node.Config = cfg
	}
	if t.Node.Config == "" {
		// 以前这种会写一个空配置文件给 openvpn，它报到超时也说不清原因
		return fmt.Errorf("节点 %s 没有 .ovpn 配置", t.Node.HostName)
	}
	if err := t.setupNetns(); err != nil {
		return err
	}
	if err := t.startOpenVPN(m.workDir); err != nil {
		return err
	}
	if t.listener == nil {
		if err := t.serve(); err != nil {
			return err
		}
	}
	ip, err := t.probeExitIP()
	if err != nil {
		return err
	}
	t.ExitIP = ip
	return nil
}

// candidatesFor 以这条隧道当前的节点打头，后面跟上同地区的其他节点作为备选。
//
// 打头的一般是 t.Node：自动重连的目标是把这条出口恢复原样，先试原节点。
// 例外是它刚在自动重连里连不上过 —— 那时还把它排第一，就会形成
// “拆掉 → 又连回同一个 → 再拆”的循环（真机上同一个 hostname 每几分钟重建一次）。
// 黑名单只改优先级：同地区抽不出别人时照样回到它，不会把隧道饿死。
// 备选会避开用户手动换掉过的节点——那些是他明确不想要的 IP，
// 让重连悄悄换回去等于撤销了他的操作。
func (m *Manager) candidatesFor(t *Tunnel) []Node {
	const maxTries = 6
	first := t.Node
	avoid := t.swapAvoid()
	// 除了用户手动换掉过的，还要避开短期内连不上过的节点
	blacklist := t.autoAvoid(time.Now())
	for h := range blacklist {
		avoid[h] = true
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	used := map[string]bool{first.HostName: true}
	for _, other := range m.tunnels {
		used[other.Node.HostName] = true
	}

	// 地区决定了备选范围，缺失时先从当前列表补一次，
	// 否则会退化成"任意地区都算同区"。
	region := first.CountryCode
	if region == "" {
		for _, n := range m.nodes {
			if n.HostName == first.HostName {
				region = n.CountryCode
				break
			}
		}
	}

	// 刚连不上的原节点不再排第一，同地区有别人就先试别人
	out := []Node{}
	if !blacklist[first.HostName] {
		out = append(out, first)
	}
	for _, n := range m.nodePoolLocked() {
		if len(out) >= maxTries {
			break
		}
		if used[n.HostName] || avoid[n.HostName] {
			continue
		}
		// 地区实在拿不到时不做限制，总比连不上强
		if region != "" && n.CountryCode != region {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		// 同地区没别人了（或者全被黑名单挡掉）。这时候还是得试原节点，
		// 否则一条只剩一个候选的隧道会永远空转。
		log.Printf("出口 %d 在 %s 没有其它候选节点，还是先试 %s", t.Slot, region, first.HostName)
		out = append(out, first)
	}
	return out
}

// Stop 停掉一条隧道并释放槽位。
func (m *Manager) Stop(slot int) error {
	invalidateInbounds()
	m.mu.Lock()
	t, ok := m.tunnels[slot]
	if ok {
		delete(m.tunnels, slot)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}
	t.stop()
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.notifyPanel()
	return nil
}

// Swap 把一条隧道换到同地区的另一个节点上，端口与已分发的客户端配置保持不变。
//
// 与健康检查的自动重连不同：那边优先重连原节点（目标是恢复），
// 这里用户是嫌当前出口 IP 不好用，必须真的换一个。
//
// 换过的节点会记进历史一并避开。只排除"当前这个"是不够的：
// A 换成 B 之后 A 就空出来了，而候选是按速度排的，A 往往又排在最前面，
// 于是再点一次就换回了 A。历史让每次点击都真的换一个没用过的。
func (m *Manager) Swap(slot int) error {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}
	if t.Status == "starting" {
		return fmt.Errorf("这个出口正在连接中，稍等一下")
	}

	node, err := m.pickSwapTarget(t)
	if err != nil {
		return err
	}
	oldHost := t.Node.HostName
	t.Node = node
	m.reconnect(t, oldHost)
	return nil
}

// pickSwapTarget 给"换节点"挑下一个目标，并把该跳过的节点记进历史。
//
// 记两个：换下来的那个，以及刚挑中的这个。挑中的也记是因为真机上踩到过——
// 它连不上时会被候选列表换成别人，但它自己没进历史，
// 于是下次点换节点又从它开始试一遍，白等一轮握手超时。
func (m *Manager) pickSwapTarget(t *Tunnel) (Node, error) {
	avoid := t.swapAvoid()
	picks, err := m.pickNodes(t.Node.CountryCode, 1, avoid)
	if err != nil && len(avoid) > 1 {
		// 这个地区的节点都换过一轮了。清掉历史重新开始，
		// 总比告诉用户"没得换了"好——转一圈之后原来那些节点未必还是当初的状态。
		t.forgetSwaps()
		picks, err = m.pickNodes(t.Node.CountryCode, 1, t.swapAvoid())
	}
	if err != nil {
		return Node{}, err
	}
	t.rememberSwap(t.Node.HostName)
	t.rememberSwap(picks[0].HostName)
	return picks[0], nil
}

// StopAll 停掉所有隧道并清空状态文件。
func (m *Manager) StopAll() {
	for _, t := range m.Tunnels() {
		_ = m.Stop(t.Slot)
	}
}

// SetCred 改一条出口的 SOCKS5 凭据。cred 两个字段都为空表示随机重置。
//
// 改完要通知后端：本机 Xray 的 socks 出站里带着这套凭据，
// 不同步的话面板侧的节点会立刻连不上自己的出口。
func (m *Manager) SetCred(slot int, cred SocksCred) (SocksCred, error) {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return SocksCred{}, fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}

	if cred.User == "" && cred.Pass == "" {
		gen, err := newSocksCred()
		if err != nil {
			return SocksCred{}, err
		}
		cred = gen
	}
	if err := validateCred(cred); err != nil {
		return SocksCred{}, err
	}

	t.setCredential(cred)
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.syncCred(t)
	return cred, nil
}

// ReconcileOutbounds 在启动恢复隧道后跑一次，把后端出站对齐到当前隧道（含 SOCKS5 凭据）。
//
// 只为 3x-ui 模式而生：它的 OnTunnelsChanged 是空操作，重启不会重写面板出站，
// 而从旧版本升上来时面板里持久化的 socks 出站没有认证字段，端口一旦要认证就连不上。
// 自建模式恢复时每条隧道 up 都会重建配置，本就自洽，这里跳过免得多重启一次 Xray。
func (m *Manager) ReconcileOutbounds() {
	p, err := openPanel()
	if err != nil || p.Kind() != "3x-ui" {
		return
	}

	// 等隧道尽量都起完再重写一次，避免只覆盖到先 up 的那几条
	deadline := time.Now().Add(90 * time.Second)
	for {
		tunnels := m.Tunnels()
		if len(tunnels) == 0 {
			return
		}
		var up *Tunnel
		settled := true
		for _, t := range tunnels {
			if t.Status == "up" && up == nil {
				up = t
			}
			if t.Status == "starting" {
				settled = false
			}
		}
		if (settled || time.Now().After(deadline)) && up != nil {
			if err := m.resync(up); err != nil {
				log.Printf("启动对账面板出站失败: %v", err)
			}
			return
		}
		if settled || time.Now().After(deadline) {
			return // 全 failed，没有可写的出站
		}
		time.Sleep(2 * time.Second)
	}
}

// syncCred 把新凭据写进后端的 socks 出站。
//
// 两种后端的做法不同：自建模式整份重建配置，3x-ui 模式只改出站那一段。
// 都走 ResyncOutbound，接口语义正好是"重写这条隧道对应的出站"。
func (m *Manager) syncCred(t *Tunnel) {
	if err := m.resync(t); err != nil {
		log.Printf("同步 SOCKS5 凭据到节点链接后端失败: %v", err)
	}
}

// Shutdown 停掉运行态但保留状态文件，让下次启动能恢复同样的隧道。
func (m *Manager) Shutdown() {
	for _, t := range m.Tunnels() {
		t.stop()
	}
}

// prepareHost 打开转发开关。netns 出网依赖它。
func prepareHost() error {
	if err := cmdRun(exec.Command("sysctl", "-qw", "net.ipv4.ip_forward=1")); err != nil {
		return fmt.Errorf("开启 ip_forward 失败: %w", err)
	}
	return nil
}

// nodeInUse 判断某节点是否已被别的隧道占用。
func (m *Manager) nodeInUse(host string, exceptSlot int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for slot, t := range m.tunnels {
		if slot != exceptSlot && t.Node.HostName == host {
			return true
		}
	}
	return false
}

// rebind 在隧道换节点后，把原先指向旧节点的 3x-ui 入站改绑到新节点。
// 面板不可用时静默跳过，健康检查本身不应因此失败。
func (m *Manager) rebind(oldHost string, t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return nil
	}
	return x.Rebind(oldHost, t, m.Tunnels())
}

// resync 在节点没换但重连过之后，把 3x-ui 的出站配置刷新一遍。
// 面板不可用时静默跳过，健康检查本身不应因此失败。
func (m *Manager) resync(t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return nil
	}
	return x.ResyncOutbound(t, m.Tunnels())
}

// notifyPanel 告诉后端隧道集合变了。
//
// 自建模式下出站是由隧道列表现算出来的，不通知的话新开的出口在 Xray 里
// 没有对应的 socks 出站，绑定会指向一个不存在的 tag。接管 3x-ui 时是空操作。
// 后端不可用不该让开关出口失败，所以只记日志。
func (m *Manager) notifyPanel() {
	p, err := openPanel()
	if err != nil {
		return
	}
	if err := p.OnTunnelsChanged(m.Tunnels()); err != nil {
		log.Printf("同步节点链接后端失败: %v", err)
	}
}
