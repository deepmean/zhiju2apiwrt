// 1) 网关最终的图片模型清单（含价格）
// 2) 那几个"网页上没有"的模型，账号里到底有没有额度（额度是"能用"的直接证据）
const GW = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || ''
const TOKEN = process.env.ZH_TOKEN || ''
if (!KEY) { console.error('set GW_KEY'); process.exit(2) }

const all = (await (await fetch(`${GW}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } })).json()).data || []
const img = all.filter((m) => m.capability === 'image')
console.log(`=== 网关图片模型（${img.length} 个）===`)
console.log('模型 id'.padEnd(30) + '展示名'.padEnd(26) + '标价'.padEnd(6) + '实付'.padEnd(6) + '档位'.padEnd(8) + '上游 key / id')
console.log('-'.repeat(110))
for (const m of img.sort((a, b) => (a.effective_change_num ?? 9e9) - (b.effective_change_num ?? 9e9))) {
  console.log(
    String(m.id).padEnd(30) + String(m.name || '').padEnd(26) +
    String(m.change_num ?? '—').padEnd(6) + String(m.effective_change_num ?? '—').padEnd(6) +
    ((m.image_sizes || []).join('/') || '(未声明)').padEnd(8) +
    `${m.upstream_key ?? '-'} / ${m.upstream_id ?? '-'}`
  )
}

if (!TOKEN) { console.log('\n(未提供 ZH_TOKEN，跳过额度核对)'); process.exit(0) }
const UP = 'https://agent.zhijuai.com/quanling'
const H = {
  'content-type': 'application/json', accept: 'application/json',
  'accept-language': 'zh-CN', 'x-locale': 'zh-CN',
  origin: 'https://agent.zhijuai.com', referer: 'https://agent.zhijuai.com/',
  'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  'x-token-id': `Bearer ${TOKEN}`, 'x-common-params': 'channel=molica_web_browser',
  plat: 'web_browser', 'x-platform': 'web_browser', ver: '2.2.11',
}
const acct = JSON.parse(await (await fetch(UP + '/v1/account/get', { method: 'POST', headers: H, body: '{}' })).text())
const remain = acct.data?.subscriptions?.models_remain ?? []
const byID = new Map(remain.map((m) => [Number(m.model_id), m]))

console.log('\n=== 额度核对：模型是否有可用额度（remain>0 即账号可用）===')
console.log('模型 id'.padEnd(30) + '上游 id'.padEnd(9) + '额度'.padEnd(7) + '账号里的名字')
console.log('-'.repeat(90))
for (const m of img.sort((a, b) => String(a.id).localeCompare(String(b.id)))) {
  const id = Number(m.upstream_id ?? 0)
  const e = byID.get(id)
  const quota = e ? String(e.remain_count) : (id === 0 ? '—(id=0)' : '不在额度表')
  console.log(String(m.id).padEnd(30) + String(id || '-').padEnd(9) + quota.padEnd(7) + (e ? e.model_name : ''))
}
