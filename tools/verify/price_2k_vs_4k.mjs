// 2K 与 4K 的价格对比，以及网关现在暴露了哪些 4K 模型。
// 上游把 4K 做成独立 model_id（不是同一模型的档位），所以价格要按 id 看。
import { readFileSync } from 'node:fs'

const GW = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || ''
if (!KEY) { console.error('set GW_KEY'); process.exit(2) }

const res = await fetch(`${GW}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } })
const all = (await res.json()).data || []
const img = all.filter((m) => m.capability === 'image')

console.log(`图片模型 ${img.length} 个\n`)
console.log('模型 id'.padEnd(30) + '展示名'.padEnd(22) + '标价'.padEnd(7) + '实付'.padEnd(7) + '档位'.padEnd(10) + '额度')
console.log('-'.repeat(96))
for (const m of img.sort((a, b) => (a.effective_change_num ?? 9e9) - (b.effective_change_num ?? 9e9))) {
  const tiers = (m.image_sizes || []).join('/') || '(未声明)'
  const remain = m.quota_remain === undefined ? '-' : String(m.quota_remain)
  console.log(
    String(m.id).padEnd(30) +
    String(m.name || '').padEnd(22) +
    String(m.change_num ?? '-').padEnd(7) +
    String(m.effective_change_num ?? '-').padEnd(7) +
    tiers.padEnd(10) +
    remain
  )
}

// 2K / 4K 成对比较：按去掉 2k/4k 后的名字配对
console.log('\n=== 2K 与 4K 的价格对比（按名字配对）===')
const norm = (s) => String(s || '').replace(/[（(]?\s*[24]k\s*(高清|超清|高)?\s*[）)]?/gi, '').replace(/\s+/g, '')
const groups = new Map()
for (const m of img) {
  const key = norm(m.name) || m.id
  if (!groups.has(key)) groups.set(key, [])
  groups.get(key).push(m)
}
for (const [k, list] of groups) {
  const has2k = list.some((m) => /2k/i.test(m.id) || /2\s*k/i.test(m.name || ''))
  const has4k = list.some((m) => /4k/i.test(m.id) || /4\s*k/i.test(m.name || ''))
  if (!(has2k && has4k)) continue
  console.log(`\n  ${k}`)
  for (const m of list.sort((a, b) => String(a.id).localeCompare(String(b.id)))) {
    console.log(`    ${String(m.id).padEnd(28)} 标价=${String(m.change_num ?? '-').padEnd(6)} 实付=${String(m.effective_change_num ?? '-').padEnd(6)} 档位=${(m.image_sizes || []).join('/') || '(未声明)'}`)
  }
  const two = list.find((m) => /2k/i.test(m.id))
  const four = list.find((m) => /4k/i.test(m.id))
  if (two && four) {
    const d = (four.effective_change_num ?? 0) - (two.effective_change_num ?? 0)
    console.log(`    → 4K 比 2K ${d === 0 ? '同价' : (d > 0 ? '贵 ' + d : '便宜 ' + (-d))} 灵力（实付）`)
  }
}

console.log('\n=== 网关暴露的 4K 模型 ===')
const fourK = img.filter((m) => /4k/i.test(m.id) || /4\s*k/i.test(m.name || '') || (m.image_sizes || []).includes('4K'))
if (fourK.length) for (const m of fourK) console.log(`  ${m.id.padEnd(30)} ${m.name} 档位=${(m.image_sizes || []).join('/') || '(未声明)'} 实付=${m.effective_change_num ?? '-'}`)
else console.log('  （没有）')
