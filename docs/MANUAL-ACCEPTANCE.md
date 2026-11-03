# 人工验收证据入口

`docs/scripts/manual-evidence.mjs` 只核对显式提交的人工记录与原始证据，不执行人工验收，也不生成通过记录。没有真实执行数据时，各人工项仍为 **NOT_RUN**。

此入口不签名、不认证执行者身份，也无法判断一份自由文本记录是否真实。执行者与复核者对记录内容负责；工具保证的是字段齐全、check ID 精确匹配、证据文件非空、SHA-256 一致，以及证据绑定当前完整源码快照。不能把“校验通过”写成抗 OCR、抗 AI、完整无障碍或生产安全认证。

## 使用方式

在所有候选源文件确定后，先取得源码身份。该命令只读，不写证据记录。

```sh
node docs/scripts/manual-evidence.mjs --snapshot
```

输出的 `source` 对象需要原样填写进证据文件。执行真人测试后，把执行者、实际 UTC 时间、具体步骤与结果、原始记录路径和 SHA-256 写入 JSON。证据文件与原始记录放在仓库外，或已被忽略的 `release-artifacts/` 目录，避免证据文件进入自己的源码指纹。不要把带凭据、答案或个人敏感信息的原件加入 Git。

通过显式环境变量提交记录。仅发现某个同名文件不会自动启用它。

```sh
export EPOCHA_MANUAL_EVIDENCE=/absolute/path/manual-session/evidence.json
node docs/scripts/manual-evidence.mjs --check stage4a.human-readability
```

同一个环境变量由 `stage4a.sh`、`stage4b.sh`、`admin-ui-check.sh` 的 `manual_result` 使用。脚本调用 helper 时捕获真实退出码，再调用自己的 `pass` / `notrun` / `fail`。工程失败优先；没有人工证据不会改写为通过。

## JSON 格式

下面是格式说明，尖括号内容必须替换。示例本身不是验收证据，不能作为 PASS 输入。

```json
{
  "schema_version": 1,
  "source": {
    "commit": "<当前完整40位Git OID>",
    "snapshot_sha256": "<当前完整源码快照的64位小写SHA-256>",
    "file_count": 123
  },
  "checks": [
    {
      "id": "stage4a.human-readability",
      "executor": "<实际执行者姓名或可追溯身份>",
      "performed_at": "<实际UTC执行时间，例如2026-11-02T08:30:00.000Z>",
      "result": "PASS",
      "notes": "<设备、系统、浏览器/辅助工具版本、执行步骤、逐项结果与限制>",
      "artifacts": [
        {
          "path": "raw/readability-session.txt",
          "sha256": "<该原始记录文件的64位小写SHA-256>"
        }
      ]
    }
  ]
}
```

约束如下。

- `performed_at` 必须是真实日历日期的 UTC ISO 时间，格式为 `YYYY-MM-DDTHH:mm:ssZ` 或 `YYYY-MM-DDTHH:mm:ss.sssZ`，不能在未来。
- `executor`、`notes` 必须非空且不含控制字符；执行者不能使用 `YOUR NAME`、`TODO`、`agent` 等占位身份。工具不能验证填写者实际是谁。
- `checks` 内 ID 必须唯一。`result` 只接受 `PASS` 或 `FAIL`，没有 `force-pass`、忽略哈希或忽略源码参数。
- 每条记录至少一份原始证据。`path` 是相对于 `evidence.json` 所在目录的 `/` 分隔路径；禁止绝对路径、盘符、反斜杠、`.`、`..` 和逃逸到证据目录外的链接。原件必须是非空普通文件，不能引用 `evidence.json` 自己。
- 证据包上限 1 MiB，每份原件上限 128 MiB，每包最多 128 条检查，每条最多 32 份原件。
- 对原始证据按原始字节计算 SHA-256。修改换行或重新编码也会改变哈希。

## 当前接入的 check ID

| ID | 真人应执行的检查 |
| --- | --- |
| `stage4a.human-readability` | 按 widget 人工验收要求实际作答，记录题型、难度、成功/失败与耗时，保留盲测原始导出；不能用自动答案表代替真人作答。 |
| `stage4b.human-readability` | 完成阶段 4B 的真人可读性与盲测记录，说明样本、设备和失败情况。 |
| `adminui.http-errors` | 在隔离测试环境人工触发并检查 403、5xx 错误提示，保留操作步骤与原始记录；未能触发其中任何一支则不能填 PASS。 |
| `adminui.clipboard-write` | 实际复制后在目标应用验证剪贴板内容；仅选中或提示手动复制属于回落路径。 |
| `adminui.real-mobile` | 在真实移动设备完成登录、创建/详情、编辑保存、启停确认与退出，记录设备、系统、浏览器与结果；BiDi 窄视口不替代真机。 |

完整无障碍合规依赖尚未实现的非视觉等价通道，仍保留原门禁的 NOT_RUN，不能用人工记录宣称功能已完成。OCR 实测继续使用自己的评测入口，不在此 helper 中代填。

## 源码指纹与退出码

指纹算法与发布检查一致：`git ls-files --cached --others --exclude-standard -z` 收集已跟踪和未忽略的新文件，去重后按路径排序。每项为 `{path,sha256}`；存在的文件哈希取磁盘原始字节，已删除路径为 `null`。对该数组的紧凑 JSON UTF-8 字节再计算 SHA-256。另核对当前 HEAD 完整 OID 与文件数；仅 commit 相同并不足以接受证据。校验结束时再核对一次源码未变。

| 退出码 | 状态 | 条件 |
| --- | --- | --- |
| `0` | `PASS` | 对应执行者记录为 PASS，当前源码、字段及所有原件全部核验一致。 |
| `1` | `FAIL` | 格式、身份字段、日期、哈希、源码不符，或执行者明确记录 FAIL。 |
| `3` | `NOT_RUN` | 未显式提供证据文件、文件不存在、没有该 check ID，或缺原始证据文件。 |

输出是一行 JSON，包含 `status`、`exit_code`、`check_id` 和固定诊断 `reason`；成功时还包括执行者、时间、源码身份与已核验原件列表。不会把证据正文或原始 JSON 拼进诊断。

本入口的单元测试使用临时 Git 仓库和明确标为合成的材料，只证明校验器行为，**不构成上述任何一项真人验收通过**。
