#!/bin/bash
# Деплой на RunPod одной командой
set -e

SERVER="$1"  # 73grnxpa3zxoxl-644114f3@ssh.runpod.io
if [ -z "$SERVER" ]; then
  echo "Usage: $0 <user>@ssh.runpod.io"
  exit 1
fi

SSH_OPTS="-i ~/.ssh/id_ed25519 -o StrictHostKeyChecking=no"
REMOTE_DIR="/opt/hash256-miner-go"

echo "=== Устанавливаем зависимости на сервере ==="
ssh $SSH_OPTS "$SERVER" bash -s <<'REMOTE_SETUP'
set -e
# CUDA toolkit (если не установлен)
if ! command -v nvcc &>/dev/null; then
  apt-get update -q
  apt-get install -y cuda-toolkit-12-4 2>/dev/null || \
  apt-get install -y nvidia-cuda-toolkit 2>/dev/null || true
fi

# Go
if ! command -v go &>/dev/null; then
  wget -q https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -O /tmp/go.tar.gz
  tar -C /usr/local -xzf /tmp/go.tar.gz
  echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
  export PATH=$PATH:/usr/local/go/bin
fi

go version
nvcc --version | head -2
REMOTE_SETUP

echo "=== Копируем исходники ==="
ssh $SSH_OPTS "$SERVER" "mkdir -p $REMOTE_DIR"
rsync -avz -e "ssh $SSH_OPTS" --exclude='.git' --exclude='*.o' --exclude='*.a' --exclude='miner' \
  ./ "$SERVER:$REMOTE_DIR/"

echo "=== Устанавливаем .env ==="
if [ -f .env ]; then
  scp $SSH_OPTS .env "$SERVER:$REMOTE_DIR/.env"
else
  echo "ВНИМАНИЕ: .env не найден. Скопируй его вручную на сервер."
fi

echo "=== Собираем на сервере ==="
ssh $SSH_OPTS "$SERVER" bash -s "$REMOTE_DIR" <<'REMOTE_BUILD'
set -e
export PATH=$PATH:/usr/local/go/bin:/usr/local/cuda/bin
cd "$1"
go mod tidy
make
echo "BUILD OK: $(ls -lh miner)"
REMOTE_BUILD

echo "=== Запускаем ==="
ssh $SSH_OPTS "$SERVER" bash -s "$REMOTE_DIR" <<'REMOTE_RUN'
export PATH=$PATH:/usr/local/go/bin:/usr/local/cuda/bin
cd "$1"
# Убиваем предыдущий инстанс если есть
pkill -f './miner' 2>/dev/null || true
sleep 1
nohup ./miner >> miner.log 2>&1 &
echo "PID: $!"
echo "Логи: tail -f $1/miner.log"
REMOTE_RUN
