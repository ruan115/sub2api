# P2 实验派生镜像

最小不可变配方：在已审查、本机已有的 base digest 上放入 `/worker`。
禁止 host bind，禁止 `latest`，禁止把本镜像称为完整 runtime 或线上等价。

```sh
# linux/amd64 worker only; do not download a base image.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/p2-worker \
  ./cmd/worker
docker build --build-arg BASE=debian@sha256:<already-local-digest> \
  -f Dockerfile /path/to/context-with-worker
```

构建与实跑由 `EXECUTION_PROVIDER_LIFECYCLE_DOCKER=1` 显式打开；默认测试 skip。
不使用 `image/lab/build.py` 或 `runtimekit`。
