@echo off
setlocal
cd /d "%~dp0"

echo ==^> [1/3] 正在拉取最新代码...
git pull
if errorlevel 1 (
    echo [错误] git pull 失败，请检查网络或本地修改。
    pause
    exit /b %errorlevel%
)

echo.
echo ==^> [2/3] 正在重新构建 Docker 镜像并启动新容器...
docker compose up -d --build --remove-orphans
if errorlevel 1 (
    echo [错误] docker compose 构建或启动失败。
    pause
    exit /b %errorlevel%
)

echo.
echo ==^> [3/3] 检查容器运行状态...
docker compose ps

echo.
echo ==^> Trae2Api 更新完成！
pause
