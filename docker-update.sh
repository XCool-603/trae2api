#!/usr/bin/env bash
set -e

cd "$(dirname "$0")"

echo "==> [1/3] 正在拉取最新代码..."
git pull

echo "==> [2/3] 正在拉取或构建 Docker 镜像并启动新容器..."
if grep -E 'image:\s*ghcr\.io' docker-compose.yml >/dev/null 2>&1; then
    docker compose pull || true
fi
docker compose up -d --build --remove-orphans

echo "==> [3/3] 检查容器运行状态..."
docker compose ps

echo "==> Trae2Api 更新完成！"
