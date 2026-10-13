// 云端记录台账：记账 → 删除 → 上游消失
// run: GW_KEY=... node tools/verify/records_ledger_test.mjs
// The new flow: a request records its session, and "all" deletes only what the ledger
// knows the gateway created.
import { readFileSync } from 'node:fs'

const BASE = process.env.GW || 'http://192.168.26.1:8080'
// Credentials come from the environment: this directory is tracked, so it must not carry
// a key. Read the gateway's own key on the router with:
//   ssh <router> 'grep ^API_KEY /opt/zhiju2api/zhiju2api.conf'
const KEY = process.env.GW_KEY || ''
if (!KEY) { console.error('set GW_KEY (e.g. GW_KEY=$(ssh jcg "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf" | cut -d= -f2))'); process.exit(2) }
const H = { authorization: `Bearer ${KEY}`, 'content-type': 'application/json' }

const api = async (path, opts = {}) => {
  const res = await fetch(BASE + path, { method: opts.method || 'GET', headers: H, body: opts.body })
  const text = await res.text()
  let j = null
  try { j = JSON.parse(text) } catch { /* keep */ }
  if (!res.ok) throw new Error((j && j.error && j.error.message) || text.slice(0, 200))
  return j
}

console.log('1) 一次对话（molica41，免费）')
const chat = await api('/v1/chat/completions', {
  method: 'POST',
  body: JSON.stringify({ model: 'molica41', stream: false, messages: [{ role: 'user', content: '只回一个字：好' }] }),
})
console.log('   reply:', String(chat.choices?.[0]?.message?.content || '').slice(0, 30))

const led = await api('/admin/records')
console.log(`2) 台账: pending=${led.pending}`)
for (const r of led.data) console.log(`   ${r.kind} ${r.session_id} ${String(r.prompt || '').slice(0, 30)}`)

console.log('3) 用台账删除（all=true 只删台账里的）')
const del = await api('/admin/records/delete', { method: 'POST', body: JSON.stringify({ all: true }) })
console.log(`   deleted=${del.deleted} failed=${JSON.stringify(del.failed)} pending=${del.pending}`)

const up = await api('/admin/records?upstream=1')
const left = up.data.filter((r) => led.data.some((l) => l.session_id === r.session_id))
console.log(`4) 该会话仍在上游列表里: ${left.length}  ${left.length === 0 ? '(已删除)' : '(残留)'}`)
console.log(`   上游会话总数: ${up.data.length}`)
