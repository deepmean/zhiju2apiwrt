// 面板三个复制按钮的产物断言：每个都要能粘到它标明的那个位置，且不能污染令牌输入框
//（那个框是会被当令牌提交的，写进配置就等于"把配置当令牌推送"）。
// run: GW_KEY=... node tools/verify/panel_copy.mjs
import { spawn } from 'node:child_process'
import { readFileSync, writeFileSync, rmSync, mkdirSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'

const CHROME = process.env.CHROME || String.raw`C:\Program Files\Google\Chrome Beta\Application\chrome.exe`
const BASE = process.env.GW || 'http://192.168.26.1:8080'
const KEY = (process.env.GW_KEY ?? '').trim()
if (!KEY) { console.error('set GW_KEY (ssh <router> "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf")'); process.exit(2) }
const MODEL = process.env.MODEL || 'seedream4-image'

const OUT = new URL('./out/', import.meta.url).pathname.replace(/^\//, '')
mkdirSync(OUT, { recursive: true })
const PORT = 9799
const PROFILE = new URL('./out/cdp-panel-copy/', import.meta.url).pathname.replace(/^\//, '')
rmSync(PROFILE, { recursive: true, force: true })
mkdirSync(PROFILE, { recursive: true })

const chrome = spawn(CHROME, [`--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`, '--headless=new',
  '--no-first-run', '--no-default-browser-check', '--window-size=1280,1500', 'about:blank'], { stdio: 'ignore' })
for (let i = 0; i < 60; i++) { try { if ((await fetch(`http://127.0.0.1:${PORT}/json/version`)).ok) break } catch {} await sleep(500) }
const tab = await (await fetch(`http://127.0.0.1:${PORT}/json/new?about:blank`, { method: 'PUT' })).json()
const ws = new WebSocket(tab.webSocketDebuggerUrl)
await new Promise((res, rej) => { ws.addEventListener('open', res, { once: true }); ws.addEventListener('error', rej, { once: true }) })
let id = 1
const pending = new Map()
ws.addEventListener('message', (ev) => {
  const t = typeof ev.data === 'string' ? ev.data : Buffer.from(ev.data).toString('utf8')
  const m = JSON.parse(t)
  if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result) }
})
const send = (method, params = {}) => {
  const mid = id++
  ws.send(JSON.stringify({ id: mid, method, params }))
  return new Promise((resolve, reject) => { pending.set(mid, { resolve, reject }); setTimeout(() => { if (pending.delete(mid)) reject(new Error('timeout ' + method)) }, 30000) })
}
await send('Page.enable'); await send('Runtime.enable')
const evalJs = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true })).result?.value

await send('Page.navigate', { url: `${BASE}/panel#key=${encodeURIComponent(KEY)}` })
await sleep(11000)
console.log('select row:', await evalJs(`(function(){var tr=document.querySelector('#models tr[data-id="${MODEL}"]'); if(!tr) return 'not found'; tr.click(); return 'ok'})()`))
await sleep(600)

async function click(btn) {
  await evalJs(`document.getElementById('${btn}').click()`)
  await sleep(700)
  return evalJs(`document.getElementById('cfgbox').value`)
}
const all = await click('copyall')
const sizes = await click('copysizes')
const yaml = await click('copyyaml')
const tokUntouched = await evalJs(`document.getElementById('tokbox').value`)

console.log('\n=== 复制插件配置 JSON ===')
console.log(all)
console.log('\n=== 复制 cordis.patch.yml 片段 ===')
console.log(yaml)

const checks = []
let cfg = null
try { cfg = JSON.parse(all) } catch { /* reported below */ }
checks.push(['配置 JSON 可解析', !!cfg])
checks.push(['含 provider=openai-compat', cfg?.provider === 'openai-compat'])
checks.push(['baseURL 指向本网关 /v1', cfg?.openaiCompatBaseURL === `${BASE}/v1`])
checks.push([`model 与所选一致（${MODEL}）`, cfg?.openaiCompatModel === MODEL])
checks.push(['editFormat=multipart', cfg?.openaiCompatEditFormat === 'multipart'])
checks.push(['editSizeMode=selected（改图才认分辨率）', cfg?.openaiCompatEditSizeMode === 'selected'])

// The plugin's own validator rules, applied to the table we emit.
let tableOk = true, why = ''
try {
  const t = JSON.parse(sizes)
  const keys = Object.keys(t)
  checks.push(['分辨率表含 1K 与 2K 两档', keys.length > 0 && Object.values(t).every((x) => x['1K'] && x['2K'])])
  for (const [ratio, tiers] of Object.entries(t)) {
    if (!/^\d+(?:\.\d+)?:\d+(?:\.\d+)?$/.test(ratio)) { tableOk = false; why = 'bad ratio ' + ratio; break }
    for (const [tier, size] of Object.entries(tiers)) {
      if (!/^(\d+(?:\.\d+)?)(K?)$/.test(tier) || typeof size !== 'string' || !size.trim()) { tableOk = false; why = `bad ${ratio}/${tier}`; break }
    }
  }
} catch (e) { tableOk = false; why = e.message }
checks.push(['分辨率表通过插件校验规则' + (tableOk ? '' : ' — ' + why), tableOk])
// Values are emitted JSON-quoted on purpose: YAML needs the quotes for an empty string
// (unquoted "" parses as null), and quoting everything keeps the rest unambiguous.
checks.push(['YAML 含 config 段与关键键',
  /- id: image-gen/.test(yaml) && /config:/.test(yaml) &&
  /openaiCompatSizes: \{/.test(yaml) && /openaiCompatEditSizeMode: "selected"/.test(yaml) &&
  /openaiCompatQuality: ""/.test(yaml)])
checks.push(['没有污染令牌输入框', tokUntouched === ''])

console.log('\n判定：')
let pass = true
for (const [name, ok] of checks) { if (!ok) pass = false; console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${name}`) }

// Scroll the block into view first: a clip of an off-screen element captures an empty region.
await evalJs(`document.getElementById('cfgbox').scrollIntoView({block:'center'}); 'ok'`)
await sleep(800)
const shot = await send('Page.captureScreenshot', { format: 'jpeg', quality: 75 })
writeFileSync(`${OUT}/panel-copy.jpg`, Buffer.from(shot.data, 'base64'))
console.log('  截图 -> tools/verify/out/panel-copy.jpg')
console.log(pass ? '\n全部通过' : '\n有失败项')
ws.close(); try { chrome.kill() } catch {}
process.exit(pass ? 0 : 1)
