#!/usr/bin/env bash
set -e

cd "$(dirname "$0")"

echo "==> [1/3] 正在拉取最新代码..."
git pull

echo "==> [2/3] 正在重新构建 Docker 镜像并启动新容器..."
docker compose up -d --build --remove-orphans

echo "==> [3/3] 检查容器运行状态..."
docker compose ps

echo "==> Trae2Api 更新完成！"
