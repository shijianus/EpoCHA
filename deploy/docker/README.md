# deploy/docker

阶段 5 交付物：`Dockerfile`（多阶段构建、非 root、`--db` 挂卷持久化）与 `docker-compose.yml`。
验收：`make docker-run` 后 `docs/scripts/stage1.sh` 全流程仍通过（DESIGN §17 阶段 5）。

1 vCPU / 1GB 机器的配套约束（DESIGN §12）：`GOMEMLIMIT=56MiB`、`GOMAXPROCS=1`、
容器内存上限 128MB、swap 512M + `vm.swappiness=10`。
