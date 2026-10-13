// 上游声明了哪些模型、网关又暴露了哪些 —— 逐项对比，专门回答"面板里怎么没有 X"。
//
// 网关的目录来自四处（cfg/list 模型表、repair/params、draw 参数、账号 models_remain），
// 任何一处漏读都会表现为"某个模型在面板里看不见"。
//
// run: GW_KEY=... ZH_TOKEN=... node tools/verify/model_inventory.mjs [关键词]
const GW = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || ''
const TOKEN = process.env.ZH_TOKEN || ''
if (!KEY) { console.error('set GW_KEY'); process.exit(2) }
const FILTER = (process.argv[2] || '').toLowerCase()

// ---- 网关侧 -------------------------------------------------------------------------
const gwRes = await fetch(`${GW}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } })
const gw = (await gwRes.json()).data || []
const gwKeys = new Set()
for (const m of gw) {
  gwKeys.add(String(m.id).toLowerCase())
  if (m.upstream_key) gwKeys.add(String(m.upstream_key).toLowerCase())
  if (m.upstream_id) gwKeys.add(String(m.upstream_id).toLowerCase())
}
const show = (m) => !FILTER || JSON.stringify(m).toLowerCase().includes(FILTER)

console.log(`=== 网关 /v1/models：${gw.length} 个条目${FILTER ? `（过滤 "${FILTER}"）` : ''} ===`)
for (const m of gw.filter(show)) {
  console.log(`  ${String(m.id).padEnd(28)} ${String(m.name || '').padEnd(20)} ${String(m.capability).padEnd(6)} key=${m.upstream_key ?? '-'} id=${m.upstream_id ?? '-'}`)
}

if (!TOKEN) {
  console.log('\n(未提供 ZH_TOKEN，跳过上游侧对比)')
  process.exit(0)
}

// ---- 上游侧：把四处声明都拉下来 -------------------------------------------------------
const UP = 'https://agent.zhijuai.com/quanling'
const H = {
  'content-type': 'application/json', accept: 'application/json',
  'accept-language': 'zh-CN', 'x-locale': 'zh-CN',
  origin: 'https://agent.zhijuai.com', referer: 'https://agent.zhijuai.com/',
  'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  'x-token-id': `Bearer ${TOKEN}`, 'x-common-params': 'channel=molica_web_browser',
  plat: 'web_browser', 'x-platform': 'web_browser', ver: '2.2.11',
}
async function post(path, body, ver) {
  const h = Object.assign({}, H, ver ? { ver } : {})
  const res = await fetch(UP + path, { method: 'POST', headers: h, body: JSON.stringify(body ?? {}) })
  const text = await res.text()
  try { return JSON.parse(text) } catch { return { _text: text.slice(0, 60) } }
}

const sources = {
  'cfg/list 模型表': null,
  'repair/params': null,
  'draw 参数': null,
  'account models_remain': null,
}

const cfg = await post('/v1/aimoss/cfg/list', {}, '1.1.10')
sources['cfg/list 模型表'] = cfg

const repair = await post('/v1/aimoss/repair/params', {}, '1.1.10')
sources['repair/params'] = repair

const account = await post('/v1/account/get', {})
sources['account models_remain'] = account

// 把 JSON 里所有 (model_key/model_code/name, model_id/id) 组合抽出来
function harvest(label, obj) {
  const found = new Map()
  const walk = (node, depth) => {
    if (!node || depth > 8) return
    if (Array.isArray(node)) { node.forEach((x) => walk(x, depth + 1)); return }
    if (typeof node !== 'object') return
    const key = node.model_key ?? node.model_code ?? node.key
    const id = node.model_id ?? node.id
    const name = node.model_name ?? node.name ?? node.title
    if (key && (name || id !== undefined)) {
      const k = String(key).toLowerCase()
      if (!found.has(k)) found.set(k, { key: String(key), id, name: name ? String(name) : '' })
      else if (name && !found.get(k).name) found.get(k).name = String(name)
    }
    for (const v of Object.values(node)) walk(v, depth + 1)
  }
  walk(obj, 0)
  return found
}

console.log('\n=== 上游声明（四处来源合并）===')
const upKeys = new Map()
for (const [label, obj] of Object.entries(sources)) {
  const got = harvest(label, obj)
  for (const [k, v] of got) if (!upKeys.has(k)) upKeys.set(k, v)
  console.log(`  ${label.padEnd(24)} 解析出 ${got.size} 个 model_key`)
}

const upList = [...upKeys.values()].sort((a, b) => a.key.localeCompare(b.key))
console.log(`\n合并后上游共 ${upList.length} 个 model_key${FILTER ? `（过滤 "${FILTER}"）` : ''}`)
for (const v of upList) {
  if (FILTER && !`${v.key} ${v.name}`.toLowerCase().includes(FILTER)) continue
  const inGw = gwKeys.has(v.key.toLowerCase())
  console.log(`  ${inGw ? '网关有' : '网关缺'}  ${v.key.padEnd(26)} id=${String(v.id).padEnd(6)} ${v.name}`)
}

const missing = upList.filter((v) => (!FILTER || `${v.key} ${v.name}`.toLowerCase().includes(FILTER)) && !gwKeys.has(v.key.toLowerCase()))
console.log(`\n上游有、网关没有的: ${missing.length}`)
for (const v of missing) console.log(`  ${v.key}  id=${v.id}  ${v.name}`)
