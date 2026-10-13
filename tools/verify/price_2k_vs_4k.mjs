// 2K 与 4K 的价格对比 + 网关是否暴露了 4K。
//
// 上游把 4K 做成独立 model_id（不是同一模型的档位），所以价格必须按 id 问。
// 问价的正确形状是 {group_key, content_type, voice_key, model_id, capacities} ——
// 用 model_code/ty 问会一律得到 ret=500（"服务繁忙"），那是形状不对，不是没有价目。
//
// run: GW_KEY=... [ZH_TOKEN=...] node tools/verify/price_2k_vs_4k.mjs
import { readFileSync } from 'node:fs'

const GW = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || ''
const TOKEN = process.env.ZH_TOKEN || ''
if (!KEY) { console.error('set GW_KEY'); process.exit(2) }

// ---- 网关侧 ---------------------------------------------------------------------------
const all = (await (await fetch(`${GW}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } })).json()).data || []
const img = all.filter((m) => m.capability === 'image')
console.log(`网关图片模型 ${img.length} 个\n`)
console.log('模型 id'.padEnd(30) + '展示名'.padEnd(22) + '标价'.padEnd(7) + '实付'.padEnd(7) + '档位'.padEnd(9) + '额度')
console.log('-'.repeat(94))
for (const m of img.sort((a, b) => (a.effective_change_num ?? 9e9) - (b.effective_change_num ?? 9e9))) {
  console.log(
    String(m.id).padEnd(30) +
    String(m.name || '').padEnd(22) +
    String(m.change_num ?? '-').padEnd(7) +
    String(m.effective_change_num ?? '-').padEnd(7) +
    ((m.image_sizes || []).join('/') || '(未声明)').padEnd(9) +
    (m.quota_remain === undefined ? '-' : m.quota_remain)
  )
}
const four = img.filter((m) => /4k/i.test(m.id) || /4\s*k/i.test(m.name || '') || (m.image_sizes || []).includes('4K'))
console.log(`\n网关暴露的 4K 模型: ${four.length}`)
for (const m of four) console.log(`  ${m.id.padEnd(30)} ${m.name} 档位=${(m.image_sizes || []).join('/') || '(未声明)'}`)

if (!TOKEN) { console.log('\n(未提供 ZH_TOKEN，跳过上游报价)'); process.exit(0) }

// ---- 上游侧：按正确形状问 2K / 4K 的价 ------------------------------------------------
const UP = 'https://agent.zhijuai.com/quanling'
const H = {
  'content-type': 'application/json', accept: 'application/json',
  'accept-language': 'zh-CN', 'x-locale': 'zh-CN',
  origin: 'https://agent.zhijuai.com', referer: 'https://agent.zhijuai.com/',
  'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  'x-token-id': `Bearer ${TOKEN}`, 'x-common-params': 'channel=molica_web_browser',
  plat: 'web_browser', 'x-platform': 'web_browser', ver: '2.2.11',
}
const post = async (p, b, ver) => {
  const h = Object.assign({}, H, ver ? { ver } : {})
  const t = await (await fetch(UP + p, { method: 'POST', headers: h, body: JSON.stringify(b ?? {}) })).text()
  try { return JSON.parse(t) } catch { return { _text: t.slice(0, 60) } }
}

// cfg/list 给出 (key, id, name)
const cfg = await post('/v1/aimoss/cfg/list', {}, '1.1.10')
const table = []
const walk = (n, d) => {
  if (!n || d > 14) return
  if (Array.isArray(n)) { n.forEach((x) => walk(x, d + 1)); return }
  if (typeof n !== 'object') return
  if (n.model_key && n.model_id !== undefined && Number(n.model_id) > 0) table.push({ id: Number(n.model_id), key: n.model_key, name: n.model_name || '' })
  for (const v of Object.values(n)) walk(v, d + 1)
}
walk(cfg.data, 0)
console.log(`\ncfg/list 模型表: ${table.length} 个带 id 的条目`)

const price = async (key, id, ct = 'draw') => {
  const p = await post('/v1/integral/getpolicy',
    { group_key: key, content_type: ct, voice_key: '', model_id: id, capacities: ct }, '1.1.10')
  if (p.ret !== 1) return { err: `ret=${p.ret} ${(p.msg || '').slice(0, 16)}` }
  const num = p.data?.change_num_result?.vip_change_num_sum
  return { num: num ?? p.data?.change_num, policy: p.data?.policy_name || '' }
}

console.log('\n=== 2K / 4K 成对报价（正确形状：group_key + model_id）===')
const families = ['nanobanana', 'nanobananaFlash2', 'gptimage25flare', 'gptimage25sunburst', 'seedream45', 'banana21']
for (const fam of families) {
  const rows = table.filter((m) => m.key.startsWith(fam)).sort((a, b) => a.id - b.id)
  if (!rows.length) continue
  console.log(`\n  ${fam}`)
  for (const m of rows) {
    const r = await price(m.key, m.id)
    console.log(`    ${String(m.id).padEnd(5)} ${String(m.key).padEnd(26)} ${String(m.name).padEnd(24)} ` +
      (r.err ? r.err : `实付=${r.num} ${r.policy.slice(0, 30)}`))
  }
  const two = rows.find((m) => /2k/i.test(m.key))
  const four2 = rows.find((m) => /4k/i.test(m.key))
  if (two && four2) {
    const a = await price(two.key, two.id)
    const b = await price(four2.key, four2.id)
    if (!a.err && !b.err) {
      const d = (b.num ?? 0) - (a.num ?? 0)
      console.log(`    → 4K(${b.num}) vs 2K(${a.num})：${d === 0 ? '同价' : d > 0 ? `贵 ${d}` : `便宜 ${-d}`} 灵力`)
    } else {
      console.log(`    → 无法比较（2K: ${a.err || a.num}, 4K: ${b.err || b.num}）`)
    }
  }
}
