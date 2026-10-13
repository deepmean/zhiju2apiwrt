package main

import (
	"net/http"
	"strings"
)

// panelHTML is the operator panel, embedded in the binary so the router serves one file
// with no external assets, no CDN and no build step.
//
// The interesting part is how a token gets in. The site is HTTPS and the router is HTTP,
// so a bookmarklet on the site's page cannot fetch the router directly (mixed content).
// Top-level navigation is not restricted, so the bookmarklet opens the panel with the
// token in the URL *fragment* — fragments never reach a server — and the panel, now
// same-origin with the gateway, pushes it with a normal request.
const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>zhiju2api 面板</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e8ee;--dim:#8b93a5;--acc:#4c8dff;--ok:#3ddc97;--warn:#ffb454;--bad:#ff6b6b}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif}
header{display:flex;align-items:baseline;gap:10px;padding:14px 18px;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--bg);z-index:2}
header b{font-size:17px}
header span{color:var(--dim);font-size:12px}
main{padding:14px 18px 60px;max-width:1180px;margin:0 auto}
section{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;margin-bottom:14px}
h3{margin:0 0 10px;font-size:14px;color:var(--dim);font-weight:600}
label{display:block;color:var(--dim);font-size:12px;margin:8px 0 4px}
input,textarea,button,select{font:inherit;color:var(--fg);background:#0d0f13;border:1px solid var(--line);border-radius:9px;padding:9px 11px;width:100%}
textarea{min-height:70px;resize:vertical;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:12px}
button{background:var(--acc);border:0;color:#fff;font-weight:600;cursor:pointer;width:auto;padding:9px 16px;text-decoration:none;display:inline-block}
button.ghost{background:#222735;color:var(--fg)}
button:disabled{opacity:.5}
a.btn{background:var(--acc);color:#fff;font-weight:600;border-radius:9px;padding:9px 16px;text-decoration:none;display:inline-block}
a.book{background:#2a3550;border:1px dashed var(--acc);color:#cfe0ff}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end}
.row>*{flex:1 1 150px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:10px}
.kv{background:#12151b;border:1px solid var(--line);border-radius:10px;padding:10px 12px}
.kv i{display:block;color:var(--dim);font-size:11px;font-style:normal;margin-bottom:3px}
.kv b{font-size:15px;font-weight:600;word-break:break-all}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--dim);font-weight:500;font-size:11.5px;position:sticky;top:0;background:var(--card)}
td.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.mono{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:11.5px}
.tag{display:inline-block;padding:0 7px;border-radius:20px;font-size:11px;border:1px solid var(--line);white-space:nowrap}
.ok{color:var(--ok);border-color:#1f5c45}.warn{color:var(--warn);border-color:#5c4a1f}.bad{color:var(--bad);border-color:#5c2323}.free{color:var(--ok)}
#msg{margin-top:10px;font-size:13px;min-height:18px;white-space:pre-wrap}
.hint{color:var(--dim);font-size:12px;margin-top:8px;line-height:1.7}
.scroll{max-height:520px;overflow:auto}
.sub{color:var(--dim);font-size:11px}
.ratios{white-space:normal;max-width:230px}
</style>
</head>
<body>
<header><b>zhiju2api</b><span id="ver"></span><span style="flex:1"></span><span id="clock"></span></header>
<main>

<section>
  <h3>接入</h3>
  <div class="row">
    <div style="flex:3 1 240px"><label>API Key（只存在本机浏览器）</label><input id="key" type="password" placeholder="sk-..."></div>
    <div style="flex:0 0 auto"><button id="save">保存</button></div>
    <div style="flex:0 0 auto"><button class="ghost" id="reload">刷新</button></div>
  </div>
  <div class="hint" id="keyState"></div>
</section>

<section>
  <h3>登录并取回令牌</h3>
  <div class="row">
    <div style="flex:0 0 auto"><a class="btn" id="sitelink" href="https://agent.zhijuai.com/" target="_blank" rel="noopener">① 打开升智登录</a></div>
    <div style="flex:0 0 auto"><a class="btn book" id="bookmarklet" href="#">② 拖我到书签栏</a></div>
  </div>
  <div class="hint">
    用法：登录站点后，在站点页面上点一下书签栏里的这个书签 —— 它会带着令牌打开本面板，
    面板自动推送并立即生效。<br>
    书签是<b>本页面现场生成</b>的（指向 <span class="mono" id="origin"></span>），换路由器地址要重新拖一次。<br>
    也可以直接在下方粘贴（DevTools → Application → Local Storage → <span class="mono">SECRET_TOKEN</span> 的 <span class="mono">data</span> 字段）。
  </div>
  <div class="row" style="margin-top:8px">
    <div style="flex:3 1 240px"><input id="tokquick" placeholder="粘贴 JWT，回车即推送"></div>
    <div style="flex:0 0 auto"><button id="pushquick">推送</button></div>
  </div>
</section>

<section>
  <h3>账号状态</h3>
  <div class="grid" id="cards"><div class="kv"><i>状态</i><b>未连接</b></div></div>
  <div class="scroll" style="margin-top:10px"><table id="tokens"></table></div>
</section>

<section>
  <h3>账号信息（服务端返回）</h3>
  <div class="grid" id="acct"><div class="kv"><i>账号</i><b>-</b></div></div>
  <div class="hint">配额 <b>不限</b> 的模型按次计费、不消耗灵力；<b>0</b> 表示额度已用完。完整列表见下方模型表。</div>
</section>

<section>
  <h3>续期（短信验证码）</h3>
  <div class="row">
    <div><label>手机号</label><input id="phone" placeholder="136xxxxxxxx" inputmode="numeric"></div>
    <div style="flex:0 0 auto"><button id="send">① 发送验证码</button></div>
  </div>
  <div class="row" style="margin-top:8px">
    <div><label>验证码</label><input id="code" placeholder="6 位数字" inputmode="numeric" autocomplete="one-time-code"></div>
    <div style="flex:0 0 auto"><button id="login">② 完成登录</button></div>
  </div>
  <div class="hint">也可以：<span class="mono">ssh 路由器 'echo 验证码 &gt; /tmp/zhiju-sms-code'</span>，网关自己完成登录。</div>
  <div id="msg"></div>
</section>

<section>
  <h3>手工替换令牌</h3>
  <textarea id="tokbox" placeholder="粘贴 JWT（每行一个）"></textarea>
  <div class="row" style="margin-top:8px"><div style="flex:0 0 auto"><button id="pushtok">推送并立即启用</button></div></div>
</section>

<section>
  <h3>模型</h3>
  <div class="row" style="margin-bottom:10px">
    <div style="flex:0 0 auto"><label style="margin:0"><input type="checkbox" id="onlyimg" checked style="width:auto"> 只看图片模型</label></div>
    <div style="flex:0 0 auto"><label style="margin:0"><input type="checkbox" id="onlyfree" style="width:auto"> 只看免费/不限</label></div>
    <div style="flex:0 0 auto"><button class="ghost" id="copycfg">复制所选模型的插件尺寸表</button></div>
  </div>
  <div class="scroll"><table id="models"></table></div>
  <div class="hint">尺寸表由上游声明的「比例 × 分辨率档位」推算（长边 = 档位 ×1024）；网关收到任何近似像素都会吸附到最近的支持比例，所以像素是参考值。</div>
</section>

</main>
<script>
var $ = function (id) { return document.getElementById(id); };
var state = { session: null, models: [], account: null, selected: null };

function api(path, opts) {
  opts = opts || {};
  var key = localStorage.getItem('zhiju_key') || '';
  var headers = { 'Authorization': 'Bearer ' + key };
  if (opts.body) headers['Content-Type'] = 'application/json';
  return fetch(path, { method: opts.method || 'GET', headers: headers, body: opts.body })
    .then(function (r) {
      return r.text().then(function (t) {
        var j = null; try { j = JSON.parse(t); } catch (e) {}
        if (!r.ok) throw new Error((j && j.error && j.error.message) || t || ('HTTP ' + r.status));
        return j;
      });
    });
}
function say(text, cls) { var el = $('msg'); el.className = cls || ''; el.textContent = text; }
function esc(s) { return String(s === undefined || s === null ? '' : s).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
function days(d) { return (d === undefined || d === null) ? '-' : (d < 0 ? '无 exp' : d + ' 天'); }
function kb(n) { if (!n) return '-'; return n >= 1000000 ? (n / 1000000).toFixed(1) + 'M' : (n >= 1000 ? Math.round(n / 1000) + 'K' : n); }

function renderSession(s) {
  var t = (s.tokens || [])[0] || {};
  var c = s.catalog || {};
  var cls = t.state === 'ok' ? 'ok' : (t.state === 'invalid' ? 'bad' : 'warn');
  var cards = [
    ['余额（灵力）', s.balance === undefined ? '-' : s.balance],
    ['令牌状态', '<span class="tag ' + cls + '">' + esc(t.state || '?') + '</span>'],
    ['剩余天数', days(t.days_left)],
    ['令牌数', s.token_count || (s.tokens || []).length],
    ['协议版本', esc(s.chat_ver || '-')],
    ['已解析模型', c.resolved || '-'],
    ['模型条目', c.openai_ids || '-']
  ];
  $('cards').innerHTML = cards.map(function (kv) { return '<div class="kv"><i>' + kv[0] + '</i><b>' + kv[1] + '</b></div>'; }).join('');
  var rows = ['<tr><th>令牌</th><th>状态</th><th>剩余</th><th>到期</th><th>最近成功</th></tr>'];
  (s.tokens || []).forEach(function (tk) {
    var cl = tk.state === 'ok' ? 'ok' : (tk.state === 'invalid' ? 'bad' : 'warn');
    rows.push('<tr><td>' + esc(tk.label) + '</td><td><span class="tag ' + cl + '">' + esc(tk.state) + '</span></td>' +
      '<td class="num">' + days(tk.days_left) + '</td><td class="mono">' + esc(tk.exp) + '</td>' +
      '<td class="mono">' + esc(tk.last_seen) + '</td></tr>');
  });
  $('tokens').innerHTML = rows.join('');
  $('ver').textContent = s.chat_ver ? ('chat ver ' + s.chat_ver) : '';
}

function renderAccount(a) {
  if (!a || a.error) { $('acct').innerHTML = '<div class="kv"><i>账号</i><b>读取失败</b></div>'; return; }
  var cards = [
    ['昵称', esc(a.nick) + ' <span class="sub">#' + esc(a.mid) + '</span>'],
    ['手机', esc(a.phone)],
    ['会员', esc(a.vip_name || '-') + ' <span class="sub">' + esc(a.vip_expire || '') + '</span>'],
    ['订阅状态', esc(a.subscription_status) + ' <span class="sub">' + (a.in_subscription ? '有效' : '无') + '</span>'],
    ['灵力余额', a.balance === undefined ? '-' : a.balance],
    ['模型额度', '不限 ' + a.models_unlimited + ' / 已用完 ' + a.models_exhausted + ' / 共 ' + a.models_total],
    ['平台', esc(a.platform || '-') + ' <span class="sub">' + esc(a.channel || '') + '</span>']
  ];
  $('acct').innerHTML = cards.map(function (kv) { return '<div class="kv"><i>' + kv[0] + '</i><b>' + kv[1] + '</b></div>'; }).join('');
}

function sizeCell(m) {
  if (!m.aspect_ratios) return '<span class="sub">—</span>';
  var parts = [];
  (m.aspect_ratios || []).forEach(function (r) {
    if (!r) return;
    var px = (m.size_table && m.size_table[r]) || {};
    var tiers = Object.keys(px).map(function (t) { return t + ' ' + px[t]; });
    parts.push('<div>' + esc(r) + (tiers.length ? ' <span class="sub">' + esc(tiers.join(' / ')) + '</span>' : '') + '</div>');
  });
  if (!parts.length) return '<span class="sub">' + esc((m.default_aspect_ratio || 'auto')) + '（仅自动）</span>';
  return '<div class="ratios">' + parts.join('') + '</div>';
}

function renderModels() {
  var onlyImg = $('onlyimg').checked, onlyFree = $('onlyfree').checked;
  var rows = ['<tr><th>模型 id</th><th>真实名字</th><th>能力</th><th class="num">标价</th><th class="num">实付</th><th class="num">配额</th><th>分辨率</th><th>支持比例 / 像素</th><th>说明</th></tr>'];
  state.models.filter(function (m) {
    if (onlyImg && m.capability === 'chat') return false;
    if (onlyFree && !(m.unlimited || m.effective_change_num === 0)) return false;
    return true;
  }).forEach(function (m) {
    var eff = m.effective_change_num;
    var effCell = (eff === undefined) ? '-' : '<span class="' + (eff === 0 ? 'free' : '') + '">' + eff + '</span>';
    var quota = (m.quota_remain === undefined) ? '-' : (m.quota_remain < 0 ? '不限' : m.quota_remain);
    var name = esc(m.name || '');
    if (m.enabled === false) name += ' <span class="tag bad">已禁用</span>';
    var tiers = (m.image_sizes || []).length ? (m.image_sizes.join('/') + '<div class="sub">默认 ' + esc(m.default_image_size || '-') + '</div>')
      : (m.capability === 'chat' ? ('<div class="sub">上下文 ' + kb(m.context_window) + '</div><div class="sub">输出 ' + kb(m.max_tokens) + (m.think ? ' · 思考' : '') + '</div>') : '<span class="sub">—</span>');
    var note = [];
    if (m.upstream_key) note.push('<span class="mono">' + esc(m.upstream_key) + '</span>');
    if (m.upstream_id) note.push('id ' + m.upstream_id);
    if (m.price_policy) note.push(esc(m.price_policy));
    if (m.price_available === false && m.price_note) note.push('<span class="warn">' + esc(m.price_note) + '</span>');
    if (m.prompt_limit) note.push('提示词 ≤' + m.prompt_limit);
    rows.push('<tr data-id="' + esc(m.id) + '"><td class="mono">' + esc(m.id) + '</td><td>' + name + '</td>' +
      '<td>' + esc(m.capability || '') + '</td><td class="num">' + (m.change_num === undefined ? '-' : m.change_num) + '</td>' +
      '<td class="num">' + effCell + '</td><td class="num">' + quota + '</td><td>' + tiers + '</td><td>' + sizeCell(m) + '</td>' +
      '<td class="sub">' + note.join('<br>') + '</td></tr>');
  });
  $('models').innerHTML = rows.join('');
  Array.prototype.forEach.call($('models').querySelectorAll('tr[data-id]'), function (tr) {
    tr.onclick = function () {
      Array.prototype.forEach.call($('models').querySelectorAll('tr'), function (x) { x.style.background = ''; });
      tr.style.background = '#1d2530';
      state.selected = tr.getAttribute('data-id');
    };
  });
}

function refresh() {
  $('clock').textContent = new Date().toLocaleTimeString();
  return Promise.all([api('/admin/session'), api('/v1/models'), api('/admin/account')])
    .then(function (r) {
      state.session = r[0]; state.models = (r[1] && r[1].data) || []; state.account = r[2];
      renderSession(r[0]); renderModels(); renderAccount(r[2]);
      $('keyState').textContent = '已连接 ' + location.host + '，每 30 秒自动刷新';
      say('');
    })
    .catch(function (e) {
      say('读取失败：' + e.message, 'bad');
      $('keyState').textContent = '请填对 API Key（路由器 /opt/zhiju2api/zhiju2api.conf 里）';
    });
}

// --- token intake ------------------------------------------------------------------
function pushTokens(list, note) {
  return api('/admin/tokens', { method: 'POST', body: JSON.stringify({ tokens: list }) })
    .then(function (r) { say((note || '已推送') + '：新增 ' + r.added + ' 个，当前 ' + r.token_count + ' 个', 'ok'); refresh(); })
    .catch(function (e) { say('推送失败：' + e.message, 'bad'); });
}

// The bookmarklet cannot call the router from the site's page (HTTPS -> HTTP is blocked
// as mixed content), but it can navigate here with the token in the fragment.
function buildBookmarklet() {
  var origin = location.origin;
  $('origin').textContent = origin;
  var js = "javascript:(function(){try{" +
    "var r=localStorage.getItem('SECRET_TOKEN');" +
    "if(!r){alert('未找到登录令牌，请先在站点登录');return}" +
    "var t;try{t=JSON.parse(r).data}catch(e){t=r}" +
    "if(!t){alert('令牌为空');return}" +
    "window.open('" + origin + "/panel#t='+encodeURIComponent(t),'_blank')" +
    "}catch(e){alert('读取失败: '+e)}})()";
  var a = $('bookmarklet');
  a.href = js;
  a.onclick = function (ev) { ev.preventDefault(); alert('把本链接拖到书签栏，然后在升智页面上点它。'); };
}

function consumeFragment() {
  var h = location.hash || '';
  if (h.indexOf('#t=') !== 0) return;
  var tok = '';
  try { tok = decodeURIComponent(h.slice(3)); } catch (e) { tok = h.slice(3); }
  history.replaceState(null, '', location.pathname);
  if (!tok) return;
  say('已从站点取回令牌，正在推送…');
  pushTokens([tok], '来自浏览器登录');
}

$('save').onclick = function () { localStorage.setItem('zhiju_key', $('key').value.trim()); refresh(); };
$('reload').onclick = refresh;
$('onlyimg').onchange = renderModels;
$('onlyfree').onchange = renderModels;
$('copycfg').onclick = function () {
  var id = state.selected;
  if (!id) { say('先在表格里点一行选择模型', 'warn'); return; }
  var m = null;
  state.models.forEach(function (x) { if (x.id === id) m = x; });
  if (!m || !m.size_table) { say('该模型没有尺寸表（不是图片模型，或上游未声明分辨率档位）', 'warn'); return; }
  var cfg = { openaiCompatBaseURL: location.origin + '/v1', openaiCompatModel: m.id, openaiCompatSizes: m.size_table };
  var text = JSON.stringify(cfg, null, 2);
  $('tokbox').value = text;
  if (navigator.clipboard) navigator.clipboard.writeText(text).catch(function () {});
  say('已生成 ' + m.id + ' 的插件配置（openaiCompatSizes），已尝试复制到剪贴板', 'ok');
};
$('send').onclick = function () {
  var phone = $('phone').value.trim();
  if (!phone) { say('请填手机号', 'warn'); return; }
  say('正在发送…');
  api('/admin/login/sms', { method: 'POST', body: JSON.stringify({ phone: phone }) })
    .then(function (r) { say('验证码已发送到 ' + (r.phone || phone) + '，10 分钟内有效', 'ok'); })
    .catch(function (e) { say('发送失败：' + e.message, 'bad'); });
};
$('login').onclick = function () {
  var code = $('code').value.trim(), phone = $('phone').value.trim();
  if (!code) { say('请填验证码', 'warn'); return; }
  say('正在登录…');
  var path = phone ? '/admin/login' : '/admin/login/code';
  api(path, { method: 'POST', body: JSON.stringify(phone ? { phone: phone, code: code } : { code: code }) })
    .then(function (r) { say('登录成功：新增 ' + r.added + ' 个令牌，当前 ' + r.token_count + ' 个', 'ok'); $('code').value = ''; refresh(); })
    .catch(function (e) { say('登录失败：' + e.message, 'bad'); });
};
$('pushtok').onclick = function () {
  var v = $('tokbox').value.trim();
  if (!v) { say('请粘贴 JWT', 'warn'); return; }
  pushTokens(v.split('\n').map(function (s) { return s.trim(); }).filter(Boolean), '已推送');
  $('tokbox').value = '';
};
$('pushquick').onclick = function () {
  var v = $('tokquick').value.trim();
  if (!v) { say('请粘贴 JWT', 'warn'); return; }
  pushTokens([v], '已推送');
  $('tokquick').value = '';
};
$('tokquick').onkeydown = function (e) { if (e.key === 'Enter') $('pushquick').onclick(); };

buildBookmarklet();
$('key').value = localStorage.getItem('zhiju_key') || '';
consumeFragment();
refresh();
setInterval(refresh, 30000);
</script>
</body>
</html>
`

// handlePanel serves the operator panel. It is not behind the API key: the page itself
// carries no secrets, and it asks for the key in the browser (kept in localStorage).
// Locking the HTML would only mean the operator cannot open the page to type the key in.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, ".css") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	_, _ = w.Write([]byte(panelHTML))
}
