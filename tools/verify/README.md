# tools/verify

一组**可重跑**的验证脚本。它们不是单元测试（那些在仓库根的 `*_test.go`，由 CI 跑），
而是**对着真网关跑**的端到端检查 —— 面板在真浏览器里的三态、插件配置能不能直接粘、
分辨率档位是不是真的生效、上游到底扣了多少钱。

这类检查值得留下的原因很直接：本项目的每个真 bug 都是"看起来正常但实际不对"，
只有拿真请求去撞才能发现。

## 准备

密钥不在仓库里（`recon/` 已清理，脚本只从环境变量读）：

```sh
export GW_KEY=$(ssh jcg 'grep ^API_KEY /opt/zhiju2api/zhiju2api.conf' | cut -d= -f2)
export GW=http://192.168.26.1:8080          # 可选，默认就是这个
export CHROME="/c/Program Files/Google/Chrome Beta/Application/chrome.exe"   # 可选
```

个别脚本要直连上游读流水，额外需要会话 JWT：

```sh
export ZH_TOKEN=$(ssh jcg 'cat /opt/zhiju2api/tokens.txt' | head -1)
```

## 脚本

| 脚本 | 验什么 | 代价 |
| --- | --- | --- |
| `panel_states.mjs` | 面板三态：无 key 必须弹门禁、`#key=` 直接进、key 错误可重填。截图存 `out/` | 免费（只读） |
| `panel_copy.mjs` | 三个复制按钮的产物：JSON 可解析、baseURL 指向本网关、分辨率表过插件校验、YAML 键齐全、**不污染令牌框** | 免费（只读） |
| `config_table_diff.mjs` | 操作者 `cordis.patch.yml` 里的分辨率表 vs 网关声明，逐项 diff + 档位与像素是否自洽 | 免费（只读） |
| `size_fix_test.mjs` | 选 2K 是不是真出 2K：**下载图片量像素**，不只看日志 | 用免费模型（升智-1） |
| `records_ledger_test.mjs` | 云端记录台账：记账 → 按台账删除 → 上游确认消失 | 一次免费对话 |
| `billing_truth.mjs` | 报价 vs 实扣：调一次免费生图，前后读余额；再拉服务端流水 | 免费生图 + 需 `ZH_TOKEN` |
| `signin_audit.mjs` | 签到/积分流水：今天到底到账多少 | 只读 + 需 `ZH_TOKEN` |
| `clean_test_records.mjs` | 按**提示词精确匹配**删掉本项目测试建的上游会话（默认只列出，加 `--delete` 才删） | 删除操作 |

## 用法示例

```sh
node tools/verify/panel_states.mjs          # 面板回归
node tools/verify/size_fix_test.mjs         # 分辨率档位回归
node tools/verify/clean_test_records.mjs    # 先看要删什么
```

## 注意

- `size_fix_test.mjs` / `billing_truth.mjs` 会**真的创建生图任务**。默认走 `seedream4-image`
  （升智-1，实测 0 灵力、额度不限）；换模型前先看 `/v1/models` 的 `effective_change_num`，
  别拿贵模型跑。
- 截图输出在 `tools/verify/out/`（已 gitignore）。
- 这些脚本用 CDP 驱动一个临时 Chrome profile，跑完自己清理；profile 也在 `out/` 下。
