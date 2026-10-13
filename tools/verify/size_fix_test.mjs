// 分辨率档位：选 2K 是否真的出 2K（实测像素，不只看日志）
// run: GW_KEY=... node tools/verify/size_fix_test.mjs
// Prove the fix end to end: ask for 2K pixels and for 1K pixels, then measure the images
// that come back. The gateway's own log line says which tier it used; this checks the
// actual pixels, which is what the complaint was about.
import { readFileSync } from 'node:fs'

const BASE = process.env.GW || 'http://192.168.26.1:8080'
// Credentials come from the environment: this directory is tracked, so it must not carry
// a key. Read the gateway's own key on the router with:
//   ssh <router> 'grep ^API_KEY /opt/zhiju2api/zhiju2api.conf'
const KEY = process.env.GW_KEY || ''
if (!KEY) { console.error('set GW_KEY (e.g. GW_KEY=$(ssh jcg "grep ^API_KEY /opt/zhiju2api/zhiju2api.conf" | cut -d= -f2))'); process.exit(2) }
const H = { authorization: `Bearer ${KEY}`, 'content-type': 'application/json' }

function dimensions(buf) {
  if (buf.subarray(0, 8).toString('hex') === '89504e470d0a1a0a') {
    return { w: buf.readUInt32BE(16), h: buf.readUInt32BE(20), fmt: 'png' }
  }
  if (buf[0] === 0xff && buf[1] === 0xd8) {
    let i = 2
    while (i < buf.length - 9) {
      if (buf[i] !== 0xff) { i++; continue }
      const marker = buf[i + 1]
      const len = buf.readUInt16BE(i + 2)
      if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc) {
        return { h: buf.readUInt16BE(i + 5), w: buf.readUInt16BE(i + 7), fmt: 'jpeg' }
      }
      i += 2 + len
    }
  }
  return null
}

async function generate(label, body) {
  const t0 = Date.now()
  const res = await fetch(`${BASE}/v1/images/generations`, { method: 'POST', headers: H, body: JSON.stringify(body) })
  const text = await res.text()
  if (!res.ok) { console.log(`  ${label}: HTTP ${res.status} ${text.slice(0, 160)}`); return }
  const url = JSON.parse(text)?.data?.[0]?.url
  const img = await fetch(url)
  const buf = Buffer.from(await img.arrayBuffer())
  const d = dimensions(buf)
  console.log(`  ${label.padEnd(28)} ${((Date.now() - t0) / 1000).toFixed(0)}s  ${d ? d.w + 'x' + d.h + ' (' + d.fmt + ')' : 'size unreadable'}  ${(buf.length / 1024).toFixed(0)}KB`)
  return d
}

console.log('=== 升智-1（seedream4-image，免费）：同一 prompt，只改 size ===')
const a = await generate('size=1024x1024 (1K)', { model: 'seedream4-image', prompt: '一只柯基犬在草地上奔跑，写实摄影', size: '1024x1024' })
const b = await generate('size=2048x2048 (2K)', { model: 'seedream4-image', prompt: '一只柯基犬在草地上奔跑，写实摄影', size: '2048x2048' })
const c = await generate('size=2048x1365 (2K 3:2)', { model: 'seedream4-image', prompt: '一只柯基犬在草地上奔跑，写实摄影', size: '2048x1365' })

console.log('\n判定：')
if (a && b) {
  console.log(`  2K 的长边 ${Math.max(b.w, b.h)} vs 1K 的长边 ${Math.max(a.w, a.h)} -> ` +
    (Math.max(b.w, b.h) > Math.max(a.w, a.h) ? '2K 确实比 1K 大 ✓' : '仍然一样大 ✗'))
} else {
  console.log('  至少一次请求没有拿到尺寸，无法比较')
}
if (c) console.log(`  3:2 请求得到 ${c.w}x${c.h}，比例 ${(c.w / c.h).toFixed(3)}（3:2 = 1.500）`)
