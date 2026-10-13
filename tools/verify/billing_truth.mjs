// 计费对账：报价与实扣对比 + 服务端流水（用 ZH_TOKEN 直连上游）
// run: GW_KEY=... node tools/verify/billing_truth.mjs
// Ground truth on cost: read the upstream consumption log, and measure the balance
// before/after one generation with the model the site labels "free" (升智-1).
import { readFileSync } from 'node:fs'

const BASE = 'https://agent.zhijuai.com/quanling'
const TOKEN = process.env.ZH_TOKEN || ''
if (!TOKEN) { console.error('set ZH_TOKEN to an upstream session JWT (see tools/zhiju_token_sync.py)'); process.exit(2) }
const H = {
  'content-type': 'application/json',
  accept: 'application/json',
  'accept-language': 'zh-CN',
  'x-locale': 'zh-CN',
  origin: 'https://agent.zhijuai.com',
  referer: 'https://agent.zhijuai.com/',
  'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  'x-token-id': `Bearer ${TOKEN}`,
  'x-common-params': 'channel=molica_web_browser',
  plat: 'web_browser',
  'x-platform': 'web_browser',
  ver: '2.2.11',
}
const post = async (p, b) => {
  const res = await fetch(BASE + p, { method: 'POST', headers: H, body: JSON.stringify(b ?? {}) })
  return { status: res.status, text: await res.text() }
}
const balance = async () => {
  const r = await post('/v1/integral/balance/query', {})
  try { return JSON.parse(r.text).data?.integral_balance } catch { return null }
}

console.log('=== 消费流水 /v1/integral/log/query ===')
for (const body of [{}, { cursor: '', limit: 20 }, { page_no: 1, page_size: 20 }, { limit: 20, cursor: '0' }]) {
  const r = await post('/v1/integral/log/query', body)
  let j = null
  try { j = JSON.parse(r.text) } catch {}
  console.log(`  body=${JSON.stringify(body)} -> ret=${j?.ret} msg=${(j?.msg || '').slice(0, 30)}`)
  if (j?.ret === 1) {
    const items = j.data?.items ?? j.data?.list ?? []
    console.log(`  条数=${items.length}`)
    for (const it of items.slice(0, 20)) console.log('   ', JSON.stringify(it).slice(0, 200))
    break
  }
}

console.log('\n=== 前后余额对账：用「免费」标注的 升智-1 生图一次 ===')
const before = await balance()
console.log(`  before = ${before}`)
const created = await post('/v1/aimoss/repair/async/create', {
  fast_model: true, urls: [], prompt: '一张用于计费对账的测试图，白色背景上的红色方块',
  model_id: 148, model_code: 'seedream4', session_id: '', tag: '', aspect_ratio: '', image_size: '1K',
})
let j = null
try { j = JSON.parse(created.text) } catch {}
console.log(`  create ret=${j?.ret} session=${j?.data?.session_id}`)
const session = j?.data?.session_id
for (let i = 0; i < 10; i++) {
  await new Promise((r) => setTimeout(r, 8000))
  const page = await post('/v1/aimoss/repair/web/list_msg', { session_id: session, limit: 100, cursor: '0' })
  const flat = page.text
  if (flat.includes('draw_content') && /"url":"https?:/.test(flat)) { console.log(`  出图（第 ${i + 1} 次轮询）`); break }
  if (flat.includes('"failure"')) { console.log(`  任务失败（第 ${i + 1} 次轮询）`); break }
}
await new Promise((r) => setTimeout(r, 5000))
const after = await balance()
console.log(`  after  = ${after}`)
console.log(`  实际扣费 = ${before !== null && after !== null ? before - after : '未知'}`)
