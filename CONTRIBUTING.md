# uwuAOSP 开发规范

## 基础规则

- 以对应 Android 大版本的 `uwu-<version>` 分支为基线。
- 已有仓库的功能开发使用 `wip/uwu-<version>-<feature>` 分支。
- 新仓库直接创建对应的 `uwu-<version>` 分支。
- 保留上游提交历史。禁止用源码快照替代正常合并或 cherry-pick。
- 一个提交只处理一个完整问题，不混入格式化、生成文件或无关修改。

## 提交格式

提交标题使用：

```text
<scope>: <imperative summary>
```

示例：

```text
uwuCLI: Add memory-aware build scheduler
SystemUI: Add uwuQS style selector
```

标题应说明改了什么。正文只保留实现原因、兼容性影响和必要的测试结果。

## 代码规范

- 优先遵循所在 AOSP 或上游项目的现有风格。
- Go 代码必须通过 `gofmt`、`go test` 和 `go vet`。
- Shell 使用 Bash，启用 `set -euo pipefail` 或等效严格模式，并通过 `bash -n`。
- Python 使用 Python 3，保持类型、异常处理和命令退出码明确。
- Make、Soong 和产品配置保持最小修改，不复制已有构建机制。
- 用户可见文字需要检查中英文、深浅色和 Phone/Pad 差异；不相关的平台路径不得改变。

## 版权

uwuAOSP 新增源码使用以下文件头：

```text
Copyright (C) <year> The uwuAOSP Project
SPDX-License-Identifier: Apache-2.0
```

修改上游文件时保留原版权和许可证。只有新增的独立 uwuAOSP 文件使用 uwuAOSP 版权头。
SVG、XML 矢量资源不添加源码版权注释；许可证由仓库级 `LICENSE` 覆盖。

## 提交前检查

```bash
git diff --check
```

同时确认：

- 没有 `out/`、缓存、日志、模型、临时备份或构建产物。
- 没有 `.codegraph/` 及专门为它新增的 `.gitignore`。
- 没有无关设备树、manifest 或本地配置修改。
- 已执行与修改范围相符的定向测试。
- 构建、提交和推送分别执行，不用推送替代本地验证。
