package config

// Package config 负责把 flag 与环境变量装配成运行时配置（DESIGN §12、§14）。
//
// 不使用 YAML/TOML 依赖：字段少且需要与 flag 一一对应。
// 主密钥与 pepper 缺失时必须拒绝启动，不得静默降级。装载逻辑在阶段 1 实现。
