// 用独立 profile 起一个 Chrome（带 CDP），把网关的 JWT 注入 localStorage.SECRET_TOKEN，
// 然后验证站点确实登录成功。不动用户现有的默认 profile。
import { spawn } from 'node:child_process'
import { rmSync, mkdirSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'

const CHROME = process.env.CHROME || String.raw`C:\Program Files\Google\Chrome Beta\Application\chrome.exe`
const SITE = 'https://agent.zhijuai.com/'
const PORT = Number(process.env.CDP_PORT || 9333)
const PROFILE = (process.env.TEMP || '/tmp') + '/cdp-inject-profile'
const TOKEN = (process.env.ZHIJU_TOKEN || '').trim()
if (!TOKEN) { console.error('set ZHIJU_TOKEN'); process.exit(2) }

rmSync(PROFILE, { recursive: true, force: true })
mkdirSync(PROFILE, { recursive: true })

const chrome = spawn(CHROME, [
  `--remote-debugging-port=${PORT}`,
  `--user-data-dir=${PROFILE}`,
  '--no-first-run', '--no-default-browser-check',
  '--window-size=1280,900',
  SITE,
], { stdio: 'ignore' })

async function waitCdp() {
  for (let i = 0; i < 60; i++) {
    try { const r = await fetch(`http://127.0.0.1:${PORT}/json/version`); if (r.ok) return await r.json() } catch {}
    await sleep(500)
  }
  throw new Error('CDP 起不来（端口 ' + PORT + '）')
}
const ver = await waitCdp()
console.log(`浏览器: ${ver.Browser}`)

// 等出现站点标签页
let tab = null
for (let i = 0; i < 40; i++) {
  const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
  tab = list.find((t) => t.type === 'page' && t.url.includes('zhijuai.com'))
  if (tab) break
  await sleep(500)
}
if (!tab) {
  const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
  tab = list.find((t) => t.type === 'page')
}
if (!tab) throw new Error('没有可用的标签页')
console.log(`标签页: ${tab.url}`)

const ws = new WebSocket(tab.webSocketDebuggerUrl)
await new Promise((res, rej) => { ws.addEventListener('open', res, { once: true }); ws.addEventListener('error', rej, { once: true }) })
let id = 1
const pending = new Map()
ws.addEventListener('message', (ev) => {
  const text = typeof ev.data === 'string' ? ev.data : Buffer.from(ev.data).toString('utf8')
  const m = JSON.parse(text)
  if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result) }
})
const send = (method, params = {}) => {
  const mid = id++
  ws.send(JSON.stringify({ id: mid, method, params }))
  return new Promise((resolve, reject) => { pending.set(mid, { resolve, reject }); setTimeout(() => { if (pending.delete(mid)) reject(new Error('timeout ' + method)) }, 30000) })
}
const evalJs = async (expr) => (await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })).result?.value

await send('Page.enable'); await send('Runtime.enable')

// 确保在站点域上（localStorage 按 origin 隔离）
const here = await evalJs('location.origin')
if (!String(here).includes('zhijuai.com')) {
  await send('Page.navigate', { url: SITE })
  await sleep(4000)
}

// 注入：结构与站点自己的写入函数一致 {data, expire}
const ttl = 7 * 24 * 3600 * 1000
const inject = `(function(){
  var v = { data: ${JSON.stringify(TOKEN)}, expire: Date.now() + ${ttl} };
  localStorage.setItem('SECRET_TOKEN', JSON.stringify(v));
  var back = localStorage.getItem('SECRET_TOKEN');
  return JSON.stringify({ origin: location.origin, len: back ? back.length : 0, ok: !!back });
})()`
console.log('注入结果:', await evalJs(inject))

// 重新加载，让应用按新令牌启动
await send('Page.navigate', { url: SITE })
await sleep(9000)

// 验证：页面上下文里用同一枚令牌请求账号接口
const check = `(async function(){
  try {
    var r = await fetch('/quanling/v1/account/get', {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-token-id': 'Bearer ' + JSON.parse(localStorage.getItem('SECRET_TOKEN')).data,
                 'x-common-params': 'channel=molica_web_browser', plat: 'web_browser', 'x-platform': 'web_browser', ver: '2.2.11' },
      body: '{}'
    });
    var j = await r.json();
    return JSON.stringify({ http: r.status, ret: j.ret, uid: j.data && j.data.id, nick: j.data && j.data.nick, phone: j.data && j.data.phone });
  } catch (e) { return 'ERR ' + e }
})()`
console.log('账号接口:', await evalJs(check))

// 页面文本里是否出现账号信息（登录成功的直观证据）
const text = await evalJs(`document.body.innerText.replace(/\\s+/g,' ').slice(0, 400)`)
console.log('页面文本片段:', String(text).slice(0, 300))

console.log('\n窗口保持打开，你可以直接在这个窗口里操作。关闭它就会结束（profile 在 ' + PROFILE + '）')
ws.close()
// 不 kill：让用户看着这个已登录的窗口
