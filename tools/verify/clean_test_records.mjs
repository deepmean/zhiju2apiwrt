// 只删本项目测试脚本自己创建的上游会话（按提示词精确匹配），操作者自己的创作一律不动。
// 判据是提示词原文，不是"最近创建的"——后者会误删网页端正在用的会话。
// run: GW_KEY=... node tools/verify/clean_test_records.mjs            # 只列出
//      GW_KEY=... node tools/verify/clean_test_records.mjs --delete   # 真正删除
const BASE = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || ''
if (!KEY) { console.error('set GW_KEY (ssh <router> "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf")'); process.exit(2) }
const H = { authorization: `Bearer ${KEY}`, 'content-type': 'application/json' }

const TEST_PROMPTS = [
  '一只柯基犬在草地上奔跑，写实摄影',
  '一张用于计费对账的测试图，白色背景上的红色方块',
  '只回一个字：好',
]

const api = async (path, opts = {}) => {
  const res = await fetch(BASE + path, { method: opts.method || 'GET', headers: H, body: opts.body })
  const text = await res.text()
  let j = null
  try { j = JSON.parse(text) } catch { /* keep */ }
  if (!res.ok) throw new Error((j?.error?.message) || text.slice(0, 200))
  return j
}

const rec = await api('/admin/records?upstream=1')
const all = rec.data || []
console.log(`上游会话总数: ${all.length}，台账待清理: ${rec.pending}`)

const mine = all.filter((r) => TEST_PROMPTS.some((p) => String(r.prompt || '').includes(p)))
const others = all.filter((r) => !mine.includes(r))
console.log(`\n匹配到本项目的测试会话: ${mine.length}`)
for (const r of mine) console.log(`  ${r.kind.padEnd(6)} ${r.session_id}  ${String(r.prompt).slice(0, 26)}`)
console.log(`\n保留（不是我建的，含操作者自己的生成）: ${others.length}`)

if (!mine.length) { console.log('\n没有需要清理的'); process.exit(0) }
if (process.argv[2] !== '--delete') {
  console.log('\n加 --delete 才真正删除（默认只列出）')
  process.exit(0)
}

const res = await api('/admin/records/delete', {
  method: 'POST',
  body: JSON.stringify({ items: mine.map((r) => ({ id: r.session_id, kind: r.kind })) }),
})
console.log(`\n删除结果: deleted=${res.deleted} failed=${JSON.stringify(res.failed)}`)
if (Object.keys(res.errors || {}).length) console.log('errors:', JSON.stringify(res.errors))

const after = await api('/admin/records?upstream=1')
const left = (after.data || []).filter((r) => mine.some((m) => m.session_id === r.session_id))
console.log(`删除后仍在上游: ${left.length}   会话总数 ${all.length} -> ${(after.data || []).length}`)
