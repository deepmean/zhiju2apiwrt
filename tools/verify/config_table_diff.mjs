// 插件配置里的分辨率表 vs 网关声明，逐项 diff
// run: GW_KEY=... node tools/verify/config_table_diff.mjs
// Compare the size table in the operator's plugin config against what the gateway says the
// model actually supports. A mismatch is invisible until an image comes back at the wrong
// resolution, so it is worth diffing rather than eyeballing.
import { readFileSync } from 'node:fs'

const CONF = 'C:/Users/ubs/.dsh/profiles/web/cordis.patch.yml'
const GW = process.env.GW || 'http://192.168.26.1:8080'
// Credentials come from the environment: this directory is tracked, so it must not carry
// a key. Read the gateway's own key on the router with:
//   ssh <router> 'grep ^API_KEY /opt/zhiju2api/zhiju2api.conf'
const KEY = process.env.GW_KEY || ''
if (!KEY) { console.error('set GW_KEY (e.g. GW_KEY=$(ssh jcg "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf" | cut -d= -f2))'); process.exit(2) }

// Pull openaiCompatSizes out of the YAML by indentation (ratio -> tier -> pixels).
function tableFromYaml(text) {
  const lines = text.split(/\r?\n/)
  const start = lines.findIndex((l) => /^\s+openaiCompatSizes:\s*$/.test(l))
  if (start < 0) return null
  const base = lines[start].match(/^\s*/)[0].length
  const out = {}
  let ratio = null
  for (let i = start + 1; i < lines.length; i++) {
    const line = lines[i]
    if (!line.trim()) continue
    const indent = line.match(/^\s*/)[0].length
    if (indent <= base) break
    const m = /^\s*(\S+):\s*(.*)$/.exec(line)
    if (!m) continue
    if (indent === base + 2) { ratio = m[1].replace(/^["']|["']$/g, ''); out[ratio] = {} }
    else if (ratio && indent >= base + 4) { out[ratio][m[1]] = m[2].replace(/^["']|["']$/g, '') }
  }
  return out
}

const conf = readFileSync(CONF, 'utf8')
const mine = tableFromYaml(conf)
const res = await fetch(`${GW}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } })
const models = (await res.json()).data
const model = models.find((m) => m.id === 'seedream4-image')
const truth = model?.size_table

if (!mine) { console.log('配置里没有 openaiCompatSizes'); process.exit(1) }
console.log(`配置里的比例数: ${Object.keys(mine).length}`)
console.log(`网关声明的比例数: ${Object.keys(truth).length}`)

const problems = []
for (const [ratio, tiers] of Object.entries(truth)) {
  const got = mine[ratio]
  if (!got) { problems.push(`缺比例 ${ratio}`); continue }
  for (const [tier, px] of Object.entries(tiers)) {
    if (!got[tier]) problems.push(`${ratio} 缺档位 ${tier}`)
    else if (got[tier] !== px) problems.push(`${ratio} ${tier}: 配置 ${got[tier]} vs 网关 ${px}`)
  }
  for (const tier of Object.keys(got)) if (!tiers[tier]) problems.push(`${ratio} 多出档位 ${tier}`)
}
for (const ratio of Object.keys(mine)) if (!truth[ratio]) problems.push(`多出比例 ${ratio}`)

// The tier a pixel size implies is what the gateway acts on now, so check that too.
const tierOf = (px) => {
  const [w, h] = px.split('x').map(Number)
  const long = Math.max(w, h)
  return long >= 512 ? Math.round(long / 1024) + 'K' : ''
}
const mislabelled = []
for (const [ratio, tiers] of Object.entries(mine)) {
  for (const [tier, px] of Object.entries(tiers)) {
    const implied = tierOf(px)
    if (implied !== tier) mislabelled.push(`${ratio} 标 ${tier} 但像素 ${px} 会被网关算成 ${implied || '（无法判定）'}`)
  }
}

console.log('\n=== 与网关声明对比 ===')
if (!problems.length) console.log('  完全一致 ✓')
else for (const p of problems) console.log('  ' + p)

console.log('\n=== 档位标注与像素是否自洽 ===')
if (!mislabelled.length) console.log('  每一档的像素都落在它自己那一档 ✓')
else for (const m of mislabelled) console.log('  ' + m)

console.log('\n=== 配置里的表 ===')
console.log(JSON.stringify(mine))
