// 面板三态检查：无 key 必须弹门禁、带 #key= 必须直接有数据、key 错误必须可重填。
// 面板曾因为"没填 key 就渲染成空壳"被误认为坏了，这三态就是锁住那个回归的。
// run: GW_KEY=... node tools/verify/panel_states.mjs
import { spawn } from 'node:child_process'
import { writeFileSync, rmSync, mkdirSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'

const CHROME = process.env.CHROME || String.raw`C:\Program Files\Google\Chrome Beta\Application\chrome.exe`
const BASE = process.env.GW || 'http://192.168.26.1:8080'
const KEY = (process.env.GW_KEY ?? '').trim()
if (!KEY) { console.error('set GW_KEY (ssh <router> "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf")'); process.exit(2) }

const OUT = new URL('./out/', import.meta.url).pathname.replace(/^\//, '')
mkdirSync(OUT, { recursive: true })
const PORT = 9666
const PROFILE = new URL('./out/cdp-panel-states/', import.meta.url).pathname.replace(/^\//, '')
rmSync(PROFILE, { recursive: true, force: true })
mkdirSync(PROFILE, { recursive: true })

const chrome = spawn(CHROME, [
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`, '--headless=new',
  '--no-first-run', '--no-default-browser-check', '--window-size=1280,1400', 'about:blank',
], { stdio: 'ignore' })

async function waitCdp() {
  for (let i = 0; i < 60; i++) {
    try { const r = await fetch(`http://127.0.0.1:${PORT}/json/version`); if (r.ok) return } catch {}
    await sleep(500)
  }
  throw new Error('CDP not up')
}
await waitCdp()
const tab = await (await fetch(`http://127.0.0.1:${PORT}/json/new?about:blank`, { method: 'PUT' })).json()
const ws = new WebSocket(tab.webSocketDebuggerUrl)
await new Promise((res, rej) => { ws.addEventListener('open', res, { once: true }); ws.addEventListener('error', rej, { once: true }) })

let id = 1
const pending = new Map()
ws.addEventListener('message', (ev) => {
  const text = typeof ev.data === 'string' ? ev.data : Buffer.from(ev.data).toString('utf8')
  const msg = JSON.parse(text)
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id); pending.delete(msg.id)
    msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result)
  }
})
const send = (method, params = {}) => {
  const mid = id++
  ws.send(JSON.stringify({ id: mid, method, params }))
  return new Promise((resolve, reject) => {
    pending.set(mid, { resolve, reject })
    setTimeout(() => { if (pending.delete(mid)) reject(new Error(`timeout ${method}`)) }, 30000)
  })
}
await send('Page.enable'); await send('Runtime.enable')
const evalJs = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true })).result?.value

async function state(label, url, pre) {
  // A fragment-only navigation is same-document and would not reload, so route each state
  // through about:blank to guarantee a fresh document.
  await send('Page.navigate', { url: `${BASE}/panel` })
  await sleep(2500)
  if (pre) await evalJs(pre)
  await send('Page.navigate', { url: 'about:blank' })
  await sleep(600)
  await send('Page.navigate', { url })
  await sleep(9000)
  const out = await evalJs(`JSON.stringify({
    gate: !document.getElementById('gate').className.includes('hide'),
    gateErr: document.getElementById('gateerr').innerText,
    cards: document.getElementById('cards').innerText.replace(/\\n/g,' | ').slice(0,120),
    rows: document.querySelectorAll('#models tr').length - 1,
    account: document.getElementById('acct').innerText.replace(/\\n/g,' | ').slice(0,90)
  })`)
  console.log(`\n[${label}] ${url}`)
  console.log('  ' + out)
  return JSON.parse(out)
}

const noKey = await state('无 key', `${BASE}/panel`, `localStorage.removeItem('zhiju_key')`)
const withKey = await state('fragment 带 key', `${BASE}/panel#key=${encodeURIComponent(KEY)}`, `localStorage.removeItem('zhiju_key')`)
const badKey = await state('错误 key', `${BASE}/panel`, `localStorage.setItem('zhiju_key','sk-wrong')`)

const shot = await send('Page.captureScreenshot', { format: 'jpeg', quality: 70, captureBeyondViewport: true })
writeFileSync(`${OUT}/panel-states.jpg`, Buffer.from(shot.data, 'base64'))

const checks = [
  ['无 key 时应弹出门禁', noKey.gate],
  ['带 key 打开时应直接有数据', !withKey.gate && withKey.rows > 0],
  ['错误 key 时应提示并可重填', badKey.gate],
]
console.log('\n判定：')
let pass = true
for (const [name, ok] of checks) { if (!ok) pass = false; console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${name}`) }
console.log(`  截图 -> tools/verify/out/panel-states.jpg`)
ws.close(); try { chrome.kill() } catch {}
// Chrome's profile is ~40MB (it downloads optimisation models on first run), so remove it
// rather than leaving it behind on every check. Windows may still hold the handles for a
// moment after the kill.
for (let i = 0; i < 10; i++) {
  await sleep(500)
  try { rmSync(PROFILE, { recursive: true, force: true }); break } catch {}
}
process.exit(pass ? 0 : 1)
