package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// aggFixture 是一个像样的 vpngate-agg 对外接口：
// 认 Bearer、按 limit/offset 翻页、把行数与总数写进响应头、支持 gzip。
type aggFixture struct {
	srv   *httptest.Server
	token string

	rows     []aggRow
	wantAuth string
	gzipBody bool
	badRows  bool // 头里的 Rows 故意比正文多，模拟响应被截断
	status   int
	offsets  []int // 收到过哪些 offset，用来验翻页
}

func newAggFixture(t *testing.T, n int) *aggFixture {
	t.Helper()
	f := &aggFixture{token: "tok-0123456789abcdef"}
	f.rows = aggRowsN(n)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vpngate/api/iphone/" {
			http.NotFound(w, r)
			return
		}
		// 真服务不会瞬间回，统计里的耗时才有意义；本地 fixture 太快会让
		// MS 取整成 0，那条断言就成了假失败
		time.Sleep(2 * time.Millisecond)
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"ok":false,"error":"x"}`)
			return
		}
		if f.wantAuth != "" && r.Header.Get("Authorization") != f.wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"ok":false,"error":"缺少或错误的 api token"}`)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		f.offsets = append(f.offsets, offset)

		page := f.rows[offset:]
		if len(page) > limit {
			page = page[:limit]
		}
		body, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		reported := len(page)
		if f.badRows {
			reported++ // 多报一行：模拟响应被中途截断
		}
		h.Set("X-Vpngate-Agg-Rows", strconv.Itoa(reported))
		h.Set("X-Vpngate-Agg-Total", strconv.Itoa(len(f.rows)))
		h.Set("Cache-Control", "private, max-age=30")

		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && f.gzipBody {
			h.Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer gz.Close()
			_, _ = gz.Write(body)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Setenv("FANOUT_AGG_API", f.srv.URL+"/vpngate/api/iphone/")
	t.Setenv("FANOUT_AGG_TOKEN", f.token)
	t.Setenv("FANOUT_AGG_FILTER", "") // 默认筛选带 level，服务端 fixture 不解析，关掉省事
	t.Cleanup(f.srv.Close)
	return f
}

func aggRowsN(n int) []aggRow {
	rows := make([]aggRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, aggRow{
			HostName:         fmt.Sprintf("vpn%06d", i),
			IP:               fmt.Sprintf("203.0.113.%d", i%250+1),
			Score:            int64(1000000 - i),
			Ping:             int64(10 + i%50),
			Speed:            int64((n - i) * 1000000),
			CountryLong:      "Japan",
			CountryShort:     "jp",
			NumVpnSessions:   3,
			OpenVPNConfigB64: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("client\nremote 203.0.113.%d 443\n", i%250+1))),
		})
	}
	return rows
}

func TestFetchAggPaginates(t *testing.T) {
	f := newAggFixture(t, 1200)
	f.wantAuth = "Bearer " + f.token

	nodes, st, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1200 {
		t.Errorf("应当翻完 1200 个，实际 %d", len(nodes))
	}
	if st.Nodes != 1200 || st.URL == "" || st.Err != "" {
		t.Errorf("统计不对: %+v", st)
	}
	if st.MS <= 0 {
		t.Errorf("耗时应当记进统计: %+v", st)
	}
	want := []int{0, 500, 1000}
	if strings.Join(toStrs(f.offsets), ",") != strings.Join(toStrs(want), ",") {
		t.Errorf("翻页步长不对，实际 %v", f.offsets)
	}
	// 国家码统一大写：candidatesFor 是按字符串比对地区的，
	// agg 给的是 "jp" 而存盘的隧道记的是 "JP"
	for _, n := range nodes[:3] {
		if n.CountryCode != "JP" {
			t.Fatalf("国家码应当大写，实际 %q", n.CountryCode)
		}
		if !strings.HasPrefix(n.Config, "client\nremote ") {
			t.Fatalf("正文没解出来: %q", n.Config)
		}
		if !n.Residential {
			t.Error("vpn<数字> 应当被认成家宽")
		}
	}
}

func TestFetchAggUsesBearerNotURLKey(t *testing.T) {
	f := newAggFixture(t, 25)
	f.wantAuth = "Bearer " + f.token
	_, st, err := fetchAgg(0)
	if err != nil {
		t.Fatalf("应当能拉下来: %v", err)
	}
	// token 出现在 URL 里就会被 journald 和界面「各源结果」抄走
	if strings.Contains(st.URL, f.token) || strings.Contains(st.URL, "key=") {
		t.Errorf("统计里的地址带着凭据: %s", st.URL)
	}
}

func TestFetchAggAuthFailureIsReadable(t *testing.T) {
	f := newAggFixture(t, 25)
	f.wantAuth = "Bearer right-token" // 配的是一个错的 token
	t.Setenv("FANOUT_AGG_TOKEN", "wrong-token")

	_, _, err := fetchAgg(0)
	if err == nil {
		t.Fatal("token 不对应当报错")
	}
	if !strings.Contains(err.Error(), "FANOUT_AGG_TOKEN") {
		t.Errorf("错误里该指出改哪个配置，实际: %v", err)
	}
}

func TestFetchAggNotFoundHint(t *testing.T) {
	f := newAggFixture(t, 25)
	f.status = http.StatusNotFound // 服务端 api.enabled=false
	_, _, err := fetchAgg(0)
	if err == nil || !strings.Contains(err.Error(), "api.enabled") {
		t.Errorf("404 应当提示服务端开关，实际: %v", err)
	}
}

func TestFetchAggFilterAndPageSize(t *testing.T) {
	f := newAggFixture(t, 300)
	t.Setenv("FANOUT_AGG_FILTER", "level=stable,reachable&limit=100&min_purity=70")
	if got := aggPageSize(); got != 100 {
		t.Errorf("limit 写在 filter 里就要按它翻页，实际 %d", got)
	}
	nodes, _, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 300 {
		t.Errorf("按 limit=100 应当翻 3 页共 300 个，实际 %d", len(nodes))
	}
	if strings.Join(toStrs(f.offsets), ",") != "0,100,200" {
		t.Errorf("翻页步长要跟服务端生效的 limit 一致，实际 %v", f.offsets)
	}
	u, err := url.Parse(aggRequestURL(0))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("level") != "stable,reachable" || u.Query().Get("min_purity") != "70" {
		t.Errorf("筛选参数没带上: %s", u.RawQuery)
	}
}

// 用户在 filter 里写了个比服务端上限还大的 limit：服务端会钉到 500，
// 这边翻页步长也得跟着钉，否则每页都在重复读同一段。
func TestAggPageSizeCappedAtServerLimit(t *testing.T) {
	t.Setenv("FANOUT_AGG_FILTER", "limit=5000")
	if got := aggPageSize(); got != aggPageLimit {
		t.Errorf("翻页步长不该超过服务端硬上限 %d，实际 %d", aggPageLimit, got)
	}
}

func TestFetchAggAcceptsGzip(t *testing.T) {
	f := newAggFixture(t, 60)
	f.gzipBody = true // 跨机器拉 6.8MB 的东西，gzip 少一半以上流量
	nodes, _, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 60 {
		t.Errorf("gzip 响应应当能正常解析，实际 %d 个", len(nodes))
	}
}

func TestFetchAggRejectsTruncatedPage(t *testing.T) {
	f := newAggFixture(t, 60)
	f.badRows = true // 头里多报一行
	_, _, err := fetchAgg(0)
	if err == nil || !strings.Contains(err.Error(), "截断") {
		t.Errorf("行数对不上应当按截断处理，实际: %v", err)
	}
}

func TestFetchAggSkipsRowsWithoutConfig(t *testing.T) {
	f := newAggFixture(t, 30)
	for i := range f.rows {
		f.rows[i].OpenVPNConfigB64 = ""
	}
	_, _, err := fetchAgg(0)
	if err == nil || !strings.Contains(err.Error(), "include_config") {
		t.Errorf("整批缺正文应当指出服务端开关，实际: %v", err)
	}
}

func TestFetchAggDropsRowWithoutConfig(t *testing.T) {
	f := newAggFixture(t, 30)
	f.rows[3].OpenVPNConfigB64 = "not-base64!!!"
	f.rows[4].OpenVPNConfigB64 = ""
	nodes, _, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 28 {
		t.Errorf("缺正文的两行该被丢掉，实际 %d", len(nodes))
	}
}

func TestFetchAggCountChecks(t *testing.T) {
	newAggFixture(t, 8) // 少于 minNodesPerSource
	if _, _, err := fetchAgg(0); err == nil || !strings.Contains(err.Error(), "过少") {
		t.Errorf("返回太少应当丢弃这个源，实际: %v", err)
	}

	newAggFixture(t, 300)
	if _, _, err := fetchAgg(900); err == nil || !strings.Contains(err.Error(), "少一半") {
		t.Errorf("比上一轮骤减一半以上应当丢弃，实际: %v", err)
	}
}

// 统计键必须不带 offset：每个 offset 各算一个源的话，
// 合理性检查的基准永远是空，骤减检测等于没做。
func TestAggStatKeyHasNoOffset(t *testing.T) {
	t.Setenv("FANOUT_AGG_API", "https://agg.example.com/vpngate/api/iphone/")
	key := aggPageStatKey()
	if strings.Contains(key, "offset=") {
		t.Errorf("统计键不该带 offset: %s", key)
	}
	if !strings.Contains(aggRequestURL(500), "offset=500") {
		t.Error("翻页时又要真的带上 offset")
	}
}

// 不带尾斜杠会被 mux 301 到带斜杠那条，而不少客户端跳转时把查询参数丢掉。
func TestAggRequestURLAddsTrailingSlash(t *testing.T) {
	t.Setenv("FANOUT_AGG_API", "https://agg.example.com/vpngate/api/iphone")
	if !strings.HasPrefix(aggRequestURL(0), "https://agg.example.com/vpngate/api/iphone/?") {
		t.Errorf("应当自己补上尾斜杠，实际 %s", aggRequestURL(0))
	}
}

func TestFetchAggDisabled(t *testing.T) {
	t.Setenv("FANOUT_AGG_API", "")
	nodes, st, err := fetchAgg(0)
	if err != nil {
		t.Fatalf("没启用这个源不算错误: %v", err)
	}
	if nodes != nil || st.URL != "" {
		t.Errorf("没启用时不该产出内容: %v %+v", nodes, st)
	}
}

func TestFetchAggHonorsMaxNodes(t *testing.T) {
	f := newAggFixture(t, 1200)
	t.Setenv("FANOUT_AGG_MAX", "600")
	nodes, _, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) > 600 {
		t.Errorf("FANOUT_AGG_MAX 应当能截断，实际 %d", len(nodes))
	}
	if len(f.offsets) > 2 {
		t.Errorf("到上限就该停，不该继续翻页: %v", f.offsets)
	}
}

// 翻页中途 agg 刷新一轮的话，窗口会飘几个：去重要兜住，
// 不能让同一个 hostname 进两次候选列表。
func TestFetchAggDedupesAcrossPages(t *testing.T) {
	f := newAggFixture(t, 1200)
	// 第二页开头复制第一页的两个节点（模拟刷新发生在两次请求之间）
	copy(f.rows[500:502], f.rows[0:2])

	nodes, _, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range nodes {
		seen[n.HostName]++
	}
	for name, c := range seen {
		if c != 1 {
			t.Fatalf("%s 出现了 %d 次", name, c)
		}
	}
	if len(nodes) != 1198 {
		t.Errorf("重复的两个应当被去掉，实际 %d", len(nodes))
	}
}

func TestFetchAggRejectsBadScheme(t *testing.T) {
	t.Setenv("FANOUT_AGG_API", "file:///etc/passwd")
	if _, _, err := fetchAgg(0); err == nil || !strings.Contains(err.Error(), "地址不对") {
		t.Errorf("非 http/https 的地址应当直接拒绝，实际: %v", err)
	}
}

func TestFetchAggBadJSONHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<!DOCTYPE html><html>…")
	}))
	t.Setenv("FANOUT_AGG_API", srv.URL+"/vpngate/api/iphone/")
	t.Setenv("FANOUT_AGG_TOKEN", "")
	t.Setenv("FANOUT_AGG_FILTER", "")
	t.Cleanup(srv.Close)

	if _, _, err := fetchAgg(0); err == nil || !strings.Contains(err.Error(), "/vpngate/api/iphone/") {
		t.Errorf("拿到 HTML 时该提示地址是不是指错了，实际: %v", err)
	}
}

// 真站联调用：设了 FANOUT_LIVE_TEST=1 又配了 FANOUT_AGG_API 才跑。
// 输出这个源单独能给多少节点、覆盖多少国家，跟官方 CSV 有多少重合。
func TestLiveAggSource(t *testing.T) {
	if os.Getenv("FANOUT_LIVE_TEST") != "1" || aggAPIBase() == "" {
		t.Skip("设 FANOUT_LIVE_TEST=1 且配好 FANOUT_AGG_API 才打真 agg")
	}
	nodes, st, err := fetchAgg(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("agg 源：%d 个节点（%dms，地址 %s）", st.Nodes, st.MS, st.URL)
	byCC := map[string]int{}
	resid := 0
	for _, n := range nodes {
		byCC[n.CountryCode]++
		if n.Residential {
			resid++
		}
	}
	t.Logf("  国家 %d 个，家宽 %d 个：%v", len(byCC), resid, byCC)

	official, err := fetchNodes(25 * time.Second)
	if err != nil {
		t.Logf("  官方主站拉取失败，没法比重合度: %v", err)
		return
	}
	in := map[string]bool{}
	for _, n := range nodes {
		in[n.HostName] = true
	}
	both := 0
	for _, n := range official {
		if in[n.HostName] {
			both++
		}
	}
	t.Logf("  官方主站 %d 个，与 agg 重合 %d 个；agg 独有 %d 个", len(official), both, len(nodes)-both)
}

func toStrs(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}
