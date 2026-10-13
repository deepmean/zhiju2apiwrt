// 签到/积分流水：今天到底到账多少（用 ZH_TOKEN 直连上游）
// run: GW_KEY=... node tools/verify/signin_audit.mjs
// Today's credits (sign-in pays out) and what the two psychic_sign config keys are.
import { readFileSync } from 'node:fs'

const BASE = 'https://agent.zhijuai.com/quanling'
const TOKEN = process.env.ZH_TOKEN || ''
if (!TOKEN) { console.error('set ZH_TOKEN to an upstream session JWT (see tools/zhiju_token_sync.py)'); process.exit(2) }
const H = {
  'content-type': 'application/json', accept: 'application/json',
  'accept-language': 'zh-CN', 'x-locale': 'zh-CN',
  origin: 'https://agent.zhijuai.com', referer: 'https://agent.zhijuai.com/',
  'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  'x-token-id': `Bearer ${TOKEN}`, 'x-common-params': 'channel=molica_web_browser',
  plat: 'web_browser', 'x-platform': 'web_browser', ver: '2.2.11',
}
async function post(path, body) {
  const res = await fetch(BASE + path, { method: 'POST', headers: H, body: JSON.stringify(body ?? {}) })
  const text = await res.text()
  try { return JSON.parse(text) } catch { return { _text: text.slice(0, 80) } }
}

const log = await post('/v1/integral/log/query', {})
const items = log.data?.items ?? []
console.log('=== 今日正向流水（加灵力）===')
for (const it of items) {
  const n = Number(it.integral_change_num) || 0
  const at = String(it.created_at || '')
  if (!at.startsWith('2026-10-13')) continue
  if (n > 0) console.log(`  ${at.slice(11, 19)}  +${String(n).padEnd(6)} bal=${it.integral_balance}  type=${it.change_type}  desc=${JSON.stringify(it.change_desc)}`)
}
console.log('\n=== 今日全部流水（含扣费，便于对账）===')
for (const it of items) {
  const at = String(it.created_at || '')
  if (!at.startsWith('2026-10-13')) continue
  const n = Number(it.integral_change_num) || 0
  console.log(`  ${at.slice(11, 19)}  ${String(n).padStart(6)}  bal=${String(it.integral_balance).padStart(7)}  type=${it.change_type}  ${JSON.stringify(it.change_desc).slice(0, 60)}`)
}

console.log('\n=== 配置里的 psychic_sign / psychic_sign_sz ===')
const cfg = readFileSync(new URL('./cfglist_ios.json', import.meta.url), 'utf8')
for (const kw of ['psychic_sign_sz', 'psychic_sign']) {
  let i = -1, n = 0
  while ((i = cfg.indexOf(kw, i + 1)) >= 0 && n < 3) {
    console.log(`--- ${kw} @${i}: ...${cfg.slice(Math.max(0, i - 260), i + 260).replace(/\s+/g, ' ')}...`)
    n++
  }
}
