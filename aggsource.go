package main

// agg 聚合中心源：读 vpngate-agg 的 GET /vpngate/api/iphone/。
//
// 为什么要有这条源：VPN Gate 官方接口一次只给按分数排的前 ~100 个（此刻实测 79 个），
// 官方镜像站又是它的缓存快照 —— 三条路拿到的是同一小批高分节点。自己搭的
// vpngate-agg 把官方主站 + sites.aspx 上全部镜像站合并成 7 天累积池，还自己
// 做了 TCP/UDP 可达与 OpenVPN 指纹探测，所以它的名单比官方那张大一个数量级。
//
// 与之前那个第三方 GitHub 池（已撤掉）的区别：
//   - 那份 .ovpn 内联在每一行里，不用按节点再去取第二个文件（所以没有
//     RemoteConfig 那条懒加载路径的事）
//   - 数据是自己那台机器拉的，不依赖第三方仓库哪天消失
//   - 代价：它是**另一台机器**的观测。level/rtt 说的是那台机器的网络，
//     对这台跑 fanout 的机器只当“这个节点还活着”的预筛，不当“连得上”的依据。
//     真能不能连仍然只有 tryNode 与健康检查说了算。
//
// 接口是 JSON 不是官方 CSV（列名对齐官方 15 列），所以这里单独一个解析器；
// 官方主站与官方镜像那条 CSV 路径不动。agg 侧不改代码：翻页、筛选、鉴权
// 都是它现成的能力。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// agg 源的默认参数。筛选默认只收“明确可达”的两档：
// level=down 的节点在 agg 那边是连续两轮明确否定（连接被拒/网络不可达），
// 收进来只会白白占掉一轮候选里的一个名额（试一个连不上的要 40 多秒）。
// 不收 fresh：fresh 只保留“上一轮官方名单里还在”的，池子会从 1200 多掉到
// 200 出头，国家覆盖也跟着掉 —— 累积池的价值恰恰在那些暂时不在名单里的。
const (
	aggDefaultFilter = "level=stable,reachable"
	aggPageLimit     = 500 // 服务端单次硬上限就是 500，超了会被钉住
	aggDefaultMax    = 1500
	// 单页解码后的体积上限。实测 500 行带正文 6.83 MB（gzip 传输出 1.24 MB，
	// 但 Go 解压后按解码字节算），所以这里必须比 CSV 那 4 MB 宽。
	aggPageMaxBytes = 12 << 20
)

// aggTransport 与 CSV 那套 nodeTransport 的区别只有一个：允许 gzip。
//
// 官方镜像站收到 Accept-Encoding: gzip 会回 200 + 0 字节（nodeTransport 里
// DisableCompression 就是为这个），但 agg 是自己的服务，压完 6.83 MB → 1.24 MB，
// 跨机器拉这个差别很大，所以这条源单独一套 transport。
var aggTransport = http.DefaultTransport.(*http.Transport).Clone()

// aggAPIBase 是 agg 的对外接口地址。空串表示不用这个源（默认）。
// 例：https://agg.example.com/vpngate/api/iphone/
func aggAPIBase() string {
	return strings.TrimSpace(os.Getenv("FANOUT_AGG_API"))
}

// aggToken 走 Authorization: Bearer，绝不拼进 URL。
//
// 不是洁癖：agg 的文档自己写了 ?key= 会落进它的 access log，而 fanout 这边
// 源 URL 会进 journald 和界面里的“各源结果”（排查节点源问题时就是靠读那些
// URL 定位的）—— token 跟在 URL 里等于随手抄进日志。
func aggToken() string {
	return strings.TrimSpace(os.Getenv("FANOUT_AGG_TOKEN"))
}

// aggFilter 是附加的查询参数，默认只收明确可达的两档。
// 想再筛别的（residential=1、country=JP、min_purity=70…）就改这个环境变量，
// 不在代码里替你猜：那些筛选项依赖 agg 那台机器探到的数据，值不值当用只有
// 部署的人知道。
func aggFilter() string {
	if v, ok := os.LookupEnv("FANOUT_AGG_FILTER"); ok {
		return strings.TrimSpace(v)
	}
	return aggDefaultFilter
}

// aggPageTimeout 是单页超时（不是整轮）。跨机器拉 6.8 MB，30 秒比 CSV 那
// 25 秒宽一点。
func aggPageTimeout() time.Duration {
	if v, ok := os.LookupEnv("FANOUT_AGG_TIMEOUT"); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// aggMaxNodes 是这一轮最多收多少个（翻页到这个数就停）。
// 实测 agg 池 1500、level=stable,reachable 之后 1244 —— 默认 1500 够用；
// 上限只为防住“哪天把池子开到几万个”那种把内存撑爆的情况。
func aggMaxNodes() int {
	if v, ok := os.LookupEnv("FANOUT_AGG_MAX"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return aggDefaultMax
}

// aggRow 是 agg 接口的一个节点，字段名与 VPN Gate 官方 15 列一致。
//
// 只声明用得上的字段：多出来的列忽略（agg 的契约里写明新增列不另行通知，
// 调用方要能容忍）。
type aggRow struct {
	HostName         string `json:"HostName"`
	IP               string `json:"IP"`
	Score            int64  `json:"Score"`
	Ping             int64  `json:"Ping"`
	Speed            int64  `json:"Speed"` // bps，跟官方一样
	CountryLong      string `json:"CountryLong"`
	CountryShort     string `json:"CountryShort"`
	NumVpnSessions   int64  `json:"NumVpnSessions"`
	OpenVPNConfigB64 string `json:"OpenVPN_ConfigData_Base64"`
}

// aggPageStatKey 是这个源的统计键（也是合理性检查的基准键）。
//
// 必须不含 offset：每个 offset 都是不同的 URL，拿它们各自当基准的话，
// 基准永远是空，“比上一轮少一半就丢弃”这条等于没做。
func aggPageStatKey() string {
	return aggRequestURL(0)
}

// aggRequestURL 拼一页的请求地址。offset=0 时不带 offset 参数，正好当统计键。
func aggRequestURL(offset int) string {
	base := aggAPIBase()
	if base == "" {
		return ""
	}
	// 不带尾斜杠的地址会被 mux 301 到带斜杠那条，而不少客户端跳转时会把查询
	// 参数（这里是筛选和 token 之外的分页）丢掉，现场很难查 —— 自己补上。
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	if f := aggFilter(); f != "" {
		for _, kv := range strings.Split(f, "&") {
			if kv = strings.TrimSpace(kv); kv != "" {
				k, v, found := strings.Cut(kv, "=")
				if found {
					q.Set(strings.TrimSpace(k), strings.TrimSpace(v))
				} else {
					q.Set(kv, "")
				}
			}
		}
	}
	if q.Get("limit") == "" {
		q.Set("limit", strconv.Itoa(aggPageLimit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// fetchAgg 翻完这个源的名单。返回的 stat 是**整个源一条**（不是每页一条），
// 界面与日志里“源 X/Y”的口径才对得上。
//
// prev 是上一轮这个源返回的节点数（按 aggPageStatKey 记），首轮或新启用
// 这个源时传 0。
// 三个返回值都起名：耗时是在 defer 里补的，匿名的话 return 已经先把
// st 拷贝走了，调用方看到的 MS 永远是 0（这个坑单元测试抓到过）。
func fetchAgg(prev int) (nodes []Node, st sourceStat, err error) {
	st = sourceStat{URL: aggPageStatKey()}
	if st.URL == "" {
		return nil, st, nil // 没启用：不是错误，也不进统计
	}
	t0 := time.Now()
	defer func() { st.MS = time.Since(t0).Milliseconds() }()

	limit := aggPageSize()
	maxNodes := aggMaxNodes()
	seen := map[string]bool{}
	var out []Node
	total := -1
	skippedNoCfg := 0

	for offset := 0; offset < maxNodes; offset += limit {
		rows, hdrTotal, err := fetchAggPage(aggRequestURL(offset), offset)
		if err != nil {
			if offset == 0 {
				return nil, st, err
			}
			// 第一页拿到了、后面某页失败：已经到手的比没有强，但要说清楚。
			// 数量合理性检查在下面统一做，掉太多会整个源丢掉。
			logfAggPartial(offset, err)
			break
		}
		if hdrTotal >= 0 {
			total = hdrTotal
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			name := strings.TrimSpace(r.HostName)
			if name == "" || seen[name] {
				continue // 翻页中途 agg 可能刷新一轮，窗口会飘几个；去重兜住
			}
			seen[name] = true
			cfg, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.OpenVPNConfigB64))
			if err != nil || len(cfg) == 0 {
				// 没有 .ovpn 的节点连不上，留着只会在候选轮里白耗 40 多秒
				skippedNoCfg++
				continue
			}
			out = append(out, Node{
				HostName:    name,
				IP:          strings.TrimSpace(r.IP),
				Country:     r.CountryLong,
				CountryCode: strings.ToUpper(strings.TrimSpace(r.CountryShort)),
				Ping:        int(r.Ping),
				SpeedMbps:   float64(r.Speed) / 1e6,
				Sessions:    int(r.NumVpnSessions),
				Config:      string(cfg),
				Residential: isResidential(name, strings.TrimSpace(r.IP)),
			})
		}
		if len(out) >= maxNodes {
			break
		}
		if total >= 0 && offset+len(rows) >= total {
			break // 翻到底了
		}
		if len(rows) < limit {
			break // 没给 Total 头时靠这个收尾
		}
	}
	// 上限是在页边沿生效的：一页 500 个可能把总数超出上限那几十个。
	// 截断而不是多拉一页，内存才真的被 FANOUT_AGG_MAX 钉住。
	if len(out) > maxNodes {
		out = out[:maxNodes]
	}

	if len(out) == 0 {
		if skippedNoCfg > 0 {
			// 整批都没正文：八成是服务端 api.include_config=false
			return nil, st, fmt.Errorf("节点列表里 %d 行都没有 .ovpn 正文（服务端 api.include_config 是不是关着？）", skippedNoCfg)
		}
		return nil, st, fmt.Errorf("聚合中心没有返回节点")
	}
	if skippedNoCfg > 0 {
		log.Printf("聚合中心有 %d 行缺 .ovpn 正文，已跳过", skippedNoCfg)
	}
	if err := nodeCountPlausible(len(out), prev); err != nil {
		return nil, st, fmt.Errorf("聚合中心: %w", err)
	}
	st.Nodes = len(out)
	return out, st, nil
}

// aggPageSize 取实际生效的单页条数：用户在 filter 里自己写了 limit 就用它，
// 否则用默认 500。翻页步长必须跟服务端实际生效的值一致，不然会漏一整段
// （limit 被服务端钉到 500、我们按 200 翻页的话每页都在重复读前 200 个）。
func aggPageSize() int {
	if f := aggFilter(); f != "" {
		for _, kv := range strings.Split(f, "&") {
			k, v, found := strings.Cut(strings.TrimSpace(kv), "=")
			if found && strings.TrimSpace(k) == "limit" {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
					if n > aggPageLimit {
						return aggPageLimit // 服务端会钉到 500，这边也得跟着钉
					}
					return n
				}
			}
		}
	}
	return aggPageLimit
}

// fetchAggPage 拉一页。返回行、X-Vpngate-Agg-Total（没有则 -1）、错误。
func fetchAggPage(rawURL string, offset int) ([]aggRow, int, error) {
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		// 这是运维自己配的地址，不挡私有网段（很可能就在内网），但 scheme
		// 只认 http/https：file:// 之类的写错法不该被当成节点源去读
		return nil, -1, fmt.Errorf("聚合中心地址不对: %s", rawURL)
	}
	client := &http.Client{Timeout: aggPageTimeout(), Transport: aggTransport}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, -1, fmt.Errorf("拉取聚合中心失败: %w", err)
	}
	if tok := aggToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("拉取聚合中心失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, -1, fmt.Errorf("聚合中心返回 HTTP %d（%s）", resp.StatusCode, aggStatusHint(resp.StatusCode))
	}
	// 多读 1 字节判断有没有超上限：截断的 JSON 会直接解析失败，但截断的
	// CSV 那类“短而完整”的坑这边也一并防住（下面的行数校验才是主力）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, aggPageMaxBytes+1))
	if err != nil {
		return nil, -1, fmt.Errorf("读取聚合中心响应失败: %w", err)
	}
	if len(raw) > aggPageMaxBytes {
		return nil, -1, fmt.Errorf("单页超过 %d 字节，按异常处理（把 FANOUT_AGG_FILTER 里的 limit 调小）", aggPageMaxBytes)
	}
	var rows []aggRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		preview := strings.Join(strings.Fields(string(raw)), " ")
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		return nil, -1, fmt.Errorf("解析聚合中心响应失败: %v（开头是 %q，确认地址指向的是 /vpngate/api/iphone/）", err, preview)
	}
	hdrTotal := -1
	if v := strings.TrimSpace(resp.Header.Get("X-Vpngate-Agg-Total")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			hdrTotal = n
		}
	}
	// 行数校验：服务端已经把自己吐了几行写在头里了，比对一下就知道响应
	// 是不是被中途截断（反代吞半截、连接断开）。这比体积阈值准得多。
	if v := strings.TrimSpace(resp.Header.Get("X-Vpngate-Agg-Rows")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != len(rows) {
			return nil, hdrTotal, fmt.Errorf("第 %d 行起应有 %d 行，实得 %d 行：响应被截断", offset, n, len(rows))
		}
	}
	return rows, hdrTotal, nil
}

// aggStatusHint 把 agg 的状态码翻译成人能照着做的话（它的错误体是 JSON，
// fanout 这边只关心该改哪个配置）。
func aggStatusHint(code int) string {
	switch code {
	case http.StatusNotFound:
		return "多半是服务端 api.enabled=false，或者地址没指到 /vpngate/api/iphone/"
	case http.StatusUnauthorized:
		return "token 不对：检查 FANOUT_AGG_TOKEN 与服务端 api.token"
	case http.StatusTooManyRequests:
		return "被服务端限流（api.rate_per_minute）"
	case http.StatusUnprocessableEntity:
		return "参数不对：检查 FANOUT_AGG_FILTER（sort 写错值不退回默认，直接 422）"
	default:
		return ""
	}
}

// logfAggPartial 记“翻到第几页挂了”。
func logfAggPartial(offset int, err error) {
	log.Printf("聚合中心翻到 offset=%d 失败，本轮只用前面拿到的: %v", offset, err)
}
