package main

import (
	"net/http"
	"strings"
)

// panelHTML is the operator panel, embedded in the binary so the router serves one file
// with no external assets, no CDN and no build step. It talks to the gateway's own API,
// which means the API key is the only thing it needs from you.
//
// Kept dependency-free on purpose: this is a 250MB router, and the page must render on a
// phone over the LAN while the desktop is off.
const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>zhiju2api 面板</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e8ee;--dim:#8b93a5;--acc:#4c8dff;--ok:#3ddc97;--warn:#ffb454;--bad:#ff6b6b}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue","PingFang SC","Microsoft YaHei",sans-serif}
header{display:flex;align-items:baseline;gap:10px;padding:16px 18px;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--bg);z-index:2}
header b{font-size:17px;letter-spacing:.3px}
header span{color:var(--dim);font-size:12px}
main{padding:14px 18px 60px;max-width:1000px;margin:0 auto}
section{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;margin-bottom:14px}
h3{margin:0 0 10px;font-size:14px;color:var(--dim);font-weight:600;letter-spacing:.4px}
label{display:block;color:var(--dim);font-size:12px;margin:8px 0 4px}
input,textarea,button,select{font:inherit;color:var(--fg);background:#0d0f13;border:1px solid var(--line);border-radius:9px;padding:9px 11px;width:100%}
textarea{min-height:64px;resize:vertical;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:12px}
button{background:var(--acc);border:0;color:#fff;font-weight:600;cursor:pointer;width:auto;padding:9px 16px}
button.ghost{background:#222735;color:var(--fg)}
button:disabled{opacity:.5;cursor:default}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end}
.row>*{flex:1 1 160px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}
.kv{background:#12151b;border:1px solid var(--line);border-radius:10px;padding:10px 12px}
.kv i{display:block;color:var(--dim);font-size:11px;font-style:normal;margin-bottom:3px}
.kv b{font-size:16px;font-weight:600}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:7px 8px;border-bottom:1px solid var(--line);white-space:nowrap}
th{color:var(--dim);font-weight:500;font-size:12px}
td.num{text-align:right;font-variant-numeric:tabular-nums}
.mono{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:12px}
.tag{display:inline-block;padding:1px 8px;border-radius:20px;font-size:11px;border:1px solid var(--line)}
.ok{color:var(--ok);border-color:#1f5c45}.warn{color:var(--warn);border-color:#5c4a1f}.bad{color:var(--bad);border-color:#5c2323}
#msg{margin-top:10px;font-size:13px;min-height:18px;white-space:pre-wrap}
.hint{color:var(--dim);font-size:12px;margin-top:8px}
.scroll{max-height:420px;overflow:auto}
.free{color:var(--ok)}
</style>
</head>
<body>
<header><b>zhiju2api</b><span id="ver"></span><span style="flex:1"></span><span id="clock"></span></header>
<main>

<section>
  <h3>接入</h3>
  <div class="row">
    <div style="flex:3 1 260px">
      <label>API Key（只存在本机浏览器）</label>
      <input id="key" type="password" placeholder="sk-...">
    </div>
    <div style="flex:0 0 auto"><button id="save">保存</button></div>
    <div style="flex:0 0 auto"><button class="ghost" id="reload">刷新</button></div>
  </div>
  <div class="hint" id="keyState"></div>
</section>

<section>
  <h3>账号状态</h3>
  <div class="grid" id="cards"><div class="kv"><i>状态</i><b>未连接</b></div></div>
  <div class="scroll" style="margin-top:10px"><table id="tokens"></table></div>
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
  <div class="hint">也可以：<span class="mono">ssh 路由器 'echo 验证码 &gt; /tmp/zhiju-sms-code'</span>，网关会自己完成登录。</div>
  <div id="msg"></div>
</section>

<section>
  <h3>手工替换令牌</h3>
  <textarea id="tokbox" placeholder="粘贴 JWT（每行一个）"></textarea>
  <div class="row" style="margin-top:8px"><div style="flex:0 0 auto"><button id="pushtok">推送并立即启用</button></div></div>
</section>

<section>
  <h3>模型（名字 · 标价 · 本账号实付 · 配额）</h3>
  <div class="row" style="margin-bottom:10px">
    <div style="flex:0 0 auto"><label style="margin:0"><input type="checkbox" id="onlyimg" checked style="width:auto"> 只看图片模型</label></div>
  </div>
  <div class="scroll"><table id="models"></table></div>
</section>

</main>
<script>
var $ = function (id) { return document.getElementById(id); };
var state = { session: null, models: null };

function api(path, opts) {
  opts = opts || {};
  var key = localStorage.getItem('zhiju_key') || '';
  var headers = { 'Authorization': 'Bearer ' + key };
  if (opts.body) headers['Content-Type'] = 'application/json';
  return fetch(path, { method: opts.method || 'GET', headers: headers, body: opts.body })
    .then(function (r) {
      return r.text().then(function (t) {
        var j = null;
        try { j = JSON.parse(t); } catch (e) {}
        if (!r.ok) {
          var m = (j && j.error && j.error.message) || t || ('HTTP ' + r.status);
          throw new Error(m);
        }
        return j;
      });
    });
}

function say(text, cls) {
  var el = $('msg');
  el.className = cls || '';
  el.textContent = text;
}

function fmtDays(d) {
  if (d === undefined || d === null) return '-';
  if (d < 0) return '无 exp';
  return d + ' 天';
}

function renderSession(s) {
  var t = (s.tokens || [])[0] || {};
  var c = s.catalog || {};
  var cls = t.state === 'ok' ? 'ok' : (t.state === 'invalid' ? 'bad' : 'warn');
  var cards = [
    ['余额（灵力）', s.balance === undefined ? '-' : s.balance],
    ['令牌状态', '<span class="tag ' + cls + '">' + (t.state || '?') + '</span>'],
    ['剩余天数', fmtDays(t.days_left)],
    ['令牌数', s.token_count || (s.tokens || []).length],
    ['协议版本', s.chat_ver || '-'],
    ['已解析模型', c.resolved || '-'],
    ['模型条目', c.openai_ids || '-']
  ];
  $('cards').innerHTML = cards.map(function (kv) {
    return '<div class="kv"><i>' + kv[0] + '</i><b>' + kv[1] + '</b></div>';
  }).join('');

  var rows = ['<tr><th>令牌</th><th>状态</th><th>剩余</th><th>到期</th><th>最近成功</th></tr>'];
  (s.tokens || []).forEach(function (tk) {
    var cl = tk.state === 'ok' ? 'ok' : (tk.state === 'invalid' ? 'bad' : 'warn');
    rows.push('<tr><td>' + (tk.label || '') + '</td>' +
      '<td><span class="tag ' + cl + '">' + (tk.state || '') + '</span></td>' +
      '<td class="num">' + fmtDays(tk.days_left) + '</td>' +
      '<td class="mono">' + (tk.exp || '') + '</td>' +
      '<td class="mono">' + (tk.last_seen || '') + '</td></tr>');
  });
  $('tokens').innerHTML = rows.join('');
  $('ver').textContent = s.chat_ver ? ('chat ver ' + s.chat_ver) : '';
}

function renderModels(list) {
  var onlyImg = $('onlyimg').checked;
  var rows = ['<tr><th>模型 id</th><th>真实名字</th><th>类型</th><th class="num">标价</th><th class="num">实付</th><th class="num">配额</th><th>比例</th></tr>'];
  list.filter(function (m) {
    return !onlyImg || m.capability !== 'chat';
  }).forEach(function (m) {
    var price = (m.change_num === undefined) ? '-' : m.change_num;
    var eff = (m.effective_change_num === undefined) ? '-' : m.effective_change_num;
    var effCls = (m.effective_change_num === 0) ? ' class="num free"' : ' class="num"';
    var quota = (m.quota_remain === undefined) ? '-' : (m.quota_remain < 0 ? '不限' : m.quota_remain);
    var ratios = (m.aspect_ratios || []).filter(function (r) { return r; }).join(' ');
    if ((m.image_sizes || []).length) ratios += ' @' + m.image_sizes.join('/');
    var name = m.name || '';
    if (m.enabled === false) name += ' <span class="tag bad">已禁用</span>';
    rows.push('<tr><td class="mono">' + m.id + '</td><td>' + name + '</td><td>' + (m.capability || '') + '</td>' +
      '<td class="num">' + price + '</td><td' + effCls + '>' + eff + '</td><td class="num">' + quota + '</td>' +
      '<td class="mono" style="white-space:normal">' + ratios + '</td></tr>');
  });
  $('models').innerHTML = rows.join('');
}

function refresh() {
  $('clock').textContent = new Date().toLocaleTimeString();
  return Promise.all([api('/admin/session'), api('/v1/models')])
    .then(function (r) {
      state.session = r[0];
      state.models = (r[1] && r[1].data) || [];
      renderSession(r[0]);
      renderModels(state.models);
      $('keyState').textContent = '已连接 ' + location.host + '，状态每 30 秒自动刷新';
      say('');
    })
    .catch(function (e) {
      say('读取状态失败：' + e.message, 'bad');
      $('keyState').textContent = '请填对 API Key（在路由器的 /opt/zhiju2api/zhiju2api.conf 里）';
    });
}

$('save').onclick = function () {
  localStorage.setItem('zhiju_key', $('key').value.trim());
  refresh();
};
$('reload').onclick = refresh;
$('onlyimg').onchange = function () { renderModels(state.models || []); };

$('send').onclick = function () {
  var phone = $('phone').value.trim();
  if (!phone) { say('请填手机号', 'warn'); return; }
  say('正在发送…');
  api('/admin/login/sms', { method: 'POST', body: JSON.stringify({ phone: phone }) })
    .then(function (r) { say('验证码已发送到 ' + (r.phone || phone) + '，请在 10 分钟内填第二步', 'ok'); })
    .catch(function (e) { say('发送失败：' + e.message, 'bad'); });
};

$('login').onclick = function () {
  var code = $('code').value.trim();
  var phone = $('phone').value.trim();
  if (!code) { say('请填验证码', 'warn'); return; }
  say('正在登录…');
  var body = phone ? { phone: phone, code: code } : { code: code };
  var path = phone ? '/admin/login' : '/admin/login/code';
  api(path, { method: 'POST', body: JSON.stringify(body) })
    .then(function (r) {
      say('登录成功：新增 ' + r.added + ' 个令牌，当前 ' + r.token_count + ' 个', 'ok');
      $('code').value = '';
      refresh();
    })
    .catch(function (e) { say('登录失败：' + e.message, 'bad'); });
};

$('pushtok').onclick = function () {
  var v = $('tokbox').value.trim();
  if (!v) { say('请粘贴 JWT', 'warn'); return; }
  var tokens = v.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
  api('/admin/tokens', { method: 'POST', body: JSON.stringify({ tokens: tokens }) })
    .then(function (r) { say('已推送 ' + r.token_count + ' 个令牌并立即生效', 'ok'); $('tokbox').value = ''; refresh(); })
    .catch(function (e) { say('推送失败：' + e.message, 'bad'); });
};

$('key').value = localStorage.getItem('zhiju_key') || '';
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
